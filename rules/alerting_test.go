// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rules

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/teststorage"
)

func TestAlertingRuleState(t *testing.T) {
	tests := []struct {
		name   string
		active map[uint64]*Alert
		want   AlertState
	}{
		{
			name: "MaxStateFiring",
			active: map[uint64]*Alert{
				0: {State: StatePending},
				1: {State: StateFiring},
			},
			want: StateFiring,
		},
		{
			name: "MaxStatePending",
			active: map[uint64]*Alert{
				0: {State: StateInactive},
				1: {State: StatePending},
			},
			want: StatePending,
		},
		{
			name: "MaxStateInactive",
			active: map[uint64]*Alert{
				0: {State: StateInactive},
				1: {State: StateInactive},
			},
			want: StateInactive,
		},
	}

	for i, test := range tests {
		rule := NewAlertingRule(test.name, nil, 0, nil, nil, nil, "", true, nil)
		rule.active = test.active
		got := rule.State()
		require.Equal(t, test.want, got, "test case %d unexpected AlertState, want:%d got:%d", i, test.want, got)
	}
}

func TestAlertingRuleLabelsUpdate(t *testing.T) {
	suite, err := promql.NewTest(t, `
		load 1m
			http_requests{job="app-server", instance="0"}	75 85 70 70
	`)
	require.NoError(t, err)
	defer suite.Close()

	require.NoError(t, suite.Run())

	expr, err := parser.ParseExpr(`http_requests < 100`)
	require.NoError(t, err)

	rule := NewAlertingRule(
		"HTTPRequestRateLow",
		expr,
		time.Minute,
		// Basing alerting rule labels off of a value that can change is a very bad idea.
		// If an alert is going back and forth between two label values it will never fire.
		// Instead, you should write two alerts with constant labels.
		labels.FromStrings("severity", "{{ if lt $value 80.0 }}critical{{ else }}warning{{ end }}"),
		nil, nil, "", true, nil,
	)

	results := []promql.Vector{
		{
			{
				Metric: labels.FromStrings(
					"__name__", "ALERTS",
					"alertname", "HTTPRequestRateLow",
					"alertstate", "pending",
					"instance", "0",
					"job", "app-server",
					"severity", "critical",
				),
				Point: promql.Point{V: 1},
			},
		},
		{
			{
				Metric: labels.FromStrings(
					"__name__", "ALERTS",
					"alertname", "HTTPRequestRateLow",
					"alertstate", "pending",
					"instance", "0",
					"job", "app-server",
					"severity", "warning",
				),
				Point: promql.Point{V: 1},
			},
		},
		{
			{
				Metric: labels.FromStrings(
					"__name__", "ALERTS",
					"alertname", "HTTPRequestRateLow",
					"alertstate", "pending",
					"instance", "0",
					"job", "app-server",
					"severity", "critical",
				),
				Point: promql.Point{V: 1},
			},
		},
		{
			{
				Metric: labels.FromStrings(
					"__name__", "ALERTS",
					"alertname", "HTTPRequestRateLow",
					"alertstate", "firing",
					"instance", "0",
					"job", "app-server",
					"severity", "critical",
				),
				Point: promql.Point{V: 1},
			},
		},
	}

	baseTime := time.Unix(0, 0)
	for i, result := range results {
		t.Logf("case %d", i)
		evalTime := baseTime.Add(time.Duration(i) * time.Minute)
		result[0].Point.T = timestamp.FromTime(evalTime)
		res, err := rule.Eval(suite.Context(), evalTime, EngineQueryFunc(suite.QueryEngine(), suite.Storage()), nil, 0)
		require.NoError(t, err)

		var filteredRes promql.Vector // After removing 'ALERTS_FOR_STATE' samples.
		for _, smpl := range res {
			smplName := smpl.Metric.Get("__name__")
			if smplName == "ALERTS" {
				filteredRes = append(filteredRes, smpl)
			} else {
				// If not 'ALERTS', it has to be 'ALERTS_FOR_STATE'.
				require.Equal(t, "ALERTS_FOR_STATE", smplName)
			}
		}

		require.Equal(t, result, filteredRes)
	}
}

func TestAlertingRuleExternalLabelsInTemplate(t *testing.T) {
	suite, err := promql.NewTest(t, `
		load 1m
			http_requests{job="app-server", instance="0"}	75 85 70 70
	`)
	require.NoError(t, err)
	defer suite.Close()

	require.NoError(t, suite.Run())

	expr, err := parser.ParseExpr(`http_requests < 100`)
	require.NoError(t, err)

	ruleWithoutExternalLabels := NewAlertingRule(
		"ExternalLabelDoesNotExist",
		expr,
		time.Minute,
		labels.FromStrings("templated_label", "There are {{ len $externalLabels }} external Labels, of which foo is {{ $externalLabels.foo }}."),
		nil,
		nil,
		"",
		true, log.NewNopLogger(),
	)
	ruleWithExternalLabels := NewAlertingRule(
		"ExternalLabelExists",
		expr,
		time.Minute,
		labels.FromStrings("templated_label", "There are {{ len $externalLabels }} external Labels, of which foo is {{ $externalLabels.foo }}."),
		nil,
		labels.FromStrings("foo", "bar", "dings", "bums"),
		"",
		true, log.NewNopLogger(),
	)
	result := promql.Vector{
		{
			Metric: labels.FromStrings(
				"__name__", "ALERTS",
				"alertname", "ExternalLabelDoesNotExist",
				"alertstate", "pending",
				"instance", "0",
				"job", "app-server",
				"templated_label", "There are 0 external Labels, of which foo is .",
			),
			Point: promql.Point{V: 1},
		},
		{
			Metric: labels.FromStrings(
				"__name__", "ALERTS",
				"alertname", "ExternalLabelExists",
				"alertstate", "pending",
				"instance", "0",
				"job", "app-server",
				"templated_label", "There are 2 external Labels, of which foo is bar.",
			),
			Point: promql.Point{V: 1},
		},
	}

	evalTime := time.Unix(0, 0)
	result[0].Point.T = timestamp.FromTime(evalTime)
	result[1].Point.T = timestamp.FromTime(evalTime)

	var filteredRes promql.Vector // After removing 'ALERTS_FOR_STATE' samples.
	res, err := ruleWithoutExternalLabels.Eval(
		suite.Context(), evalTime, EngineQueryFunc(suite.QueryEngine(), suite.Storage()), nil, 0,
	)
	require.NoError(t, err)
	for _, smpl := range res {
		smplName := smpl.Metric.Get("__name__")
		if smplName == "ALERTS" {
			filteredRes = append(filteredRes, smpl)
		} else {
			// If not 'ALERTS', it has to be 'ALERTS_FOR_STATE'.
			require.Equal(t, "ALERTS_FOR_STATE", smplName)
		}
	}

	res, err = ruleWithExternalLabels.Eval(
		suite.Context(), evalTime, EngineQueryFunc(suite.QueryEngine(), suite.Storage()), nil, 0,
	)
	require.NoError(t, err)
	for _, smpl := range res {
		smplName := smpl.Metric.Get("__name__")
		if smplName == "ALERTS" {
			filteredRes = append(filteredRes, smpl)
		} else {
			// If not 'ALERTS', it has to be 'ALERTS_FOR_STATE'.
			require.Equal(t, "ALERTS_FOR_STATE", smplName)
		}
	}

	require.Equal(t, result, filteredRes)
}

func TestAlertingRuleExternalURLInTemplate(t *testing.T) {
	suite, err := promql.NewTest(t, `
		load 1m
			http_requests{job="app-server", instance="0"}	75 85 70 70
	`)
	require.NoError(t, err)
	defer suite.Close()

	require.NoError(t, suite.Run())

	expr, err := parser.ParseExpr(`http_requests < 100`)
	require.NoError(t, err)

	ruleWithoutExternalURL := NewAlertingRule(
		"ExternalURLDoesNotExist",
		expr,
		time.Minute,
		labels.FromStrings("templated_label", "The external URL is {{ $externalURL }}."),
		nil,
		nil,
		"",
		true, log.NewNopLogger(),
	)
	ruleWithExternalURL := NewAlertingRule(
		"ExternalURLExists",
		expr,
		time.Minute,
		labels.FromStrings("templated_label", "The external URL is {{ $externalURL }}."),
		nil,
		nil,
		"http://localhost:1234",
		true, log.NewNopLogger(),
	)
	result := promql.Vector{
		{
			Metric: labels.FromStrings(
				"__name__", "ALERTS",
				"alertname", "ExternalURLDoesNotExist",
				"alertstate", "pending",
				"instance", "0",
				"job", "app-server",
				"templated_label", "The external URL is .",
			),
			Point: promql.Point{V: 1},
		},
		{
			Metric: labels.FromStrings(
				"__name__", "ALERTS",
				"alertname", "ExternalURLExists",
				"alertstate", "pending",
				"instance", "0",
				"job", "app-server",
				"templated_label", "The external URL is http://localhost:1234.",
			),
			Point: promql.Point{V: 1},
		},
	}

	evalTime := time.Unix(0, 0)
	result[0].Point.T = timestamp.FromTime(evalTime)
	result[1].Point.T = timestamp.FromTime(evalTime)

	var filteredRes promql.Vector // After removing 'ALERTS_FOR_STATE' samples.
	res, err := ruleWithoutExternalURL.Eval(
		suite.Context(), evalTime, EngineQueryFunc(suite.QueryEngine(), suite.Storage()), nil, 0,
	)
	require.NoError(t, err)
	for _, smpl := range res {
		smplName := smpl.Metric.Get("__name__")
		if smplName == "ALERTS" {
			filteredRes = append(filteredRes, smpl)
		} else {
			// If not 'ALERTS', it has to be 'ALERTS_FOR_STATE'.
			require.Equal(t, "ALERTS_FOR_STATE", smplName)
		}
	}

	res, err = ruleWithExternalURL.Eval(
		suite.Context(), evalTime, EngineQueryFunc(suite.QueryEngine(), suite.Storage()), nil, 0,
	)
	require.NoError(t, err)
	for _, smpl := range res {
		smplName := smpl.Metric.Get("__name__")
		if smplName == "ALERTS" {
			filteredRes = append(filteredRes, smpl)
		} else {
			// If not 'ALERTS', it has to be 'ALERTS_FOR_STATE'.
			require.Equal(t, "ALERTS_FOR_STATE", smplName)
		}
	}

	require.Equal(t, result, filteredRes)
}

func TestAlertingRuleEmptyLabelFromTemplate(t *testing.T) {
	suite, err := promql.NewTest(t, `
		load 1m
			http_requests{job="app-server", instance="0"}	75 85 70 70
	`)
	require.NoError(t, err)
	defer suite.Close()

	require.NoError(t, suite.Run())

	expr, err := parser.ParseExpr(`http_requests < 100`)
	require.NoError(t, err)

	rule := NewAlertingRule(
		"EmptyLabel",
		expr,
		time.Minute,
		labels.FromStrings("empty_label", ""),
		nil,
		nil,
		"",
		true, log.NewNopLogger(),
	)
	result := promql.Vector{
		{
			Metric: labels.FromStrings(
				"__name__", "ALERTS",
				"alertname", "EmptyLabel",
				"alertstate", "pending",
				"instance", "0",
				"job", "app-server",
			),
			Point: promql.Point{V: 1},
		},
	}

	evalTime := time.Unix(0, 0)
	result[0].Point.T = timestamp.FromTime(evalTime)

	var filteredRes promql.Vector // After removing 'ALERTS_FOR_STATE' samples.
	res, err := rule.Eval(
		suite.Context(), evalTime, EngineQueryFunc(suite.QueryEngine(), suite.Storage()), nil, 0,
	)
	require.NoError(t, err)
	for _, smpl := range res {
		smplName := smpl.Metric.Get("__name__")
		if smplName == "ALERTS" {
			filteredRes = append(filteredRes, smpl)
		} else {
			// If not 'ALERTS', it has to be 'ALERTS_FOR_STATE'.
			require.Equal(t, "ALERTS_FOR_STATE", smplName)
		}
	}
	require.Equal(t, result, filteredRes)
}

func TestAlertingRuleQueryInTemplate(t *testing.T) {
	suite, err := promql.NewTest(t, `
		load 1m
			http_requests{job="app-server", instance="0"}	70 85 70 70
	`)
	require.NoError(t, err)
	defer suite.Close()

	require.NoError(t, suite.Run())

	expr, err := parser.ParseExpr(`sum(http_requests) < 100`)
	require.NoError(t, err)

	ruleWithQueryInTemplate := NewAlertingRule(
		"ruleWithQueryInTemplate",
		expr,
		time.Minute,
		labels.FromStrings("label", "value"),
		labels.FromStrings("templated_label", `{{- with "sort(sum(http_requests) by (instance))" | query -}}
{{- range $i,$v := . -}}
instance: {{ $v.Labels.instance }}, value: {{ printf "%.0f" $v.Value }};
{{- end -}}
{{- end -}}
`),
		nil,
		"",
		true, log.NewNopLogger(),
	)
	evalTime := time.Unix(0, 0)

	startQueryCh := make(chan struct{})
	getDoneCh := make(chan struct{})
	slowQueryFunc := func(ctx context.Context, q string, ts time.Time) (promql.Vector, error) {
		if q == "sort(sum(http_requests) by (instance))" {
			// This is a minimum reproduction of issue 10703, expand template with query.
			close(startQueryCh)
			select {
			case <-getDoneCh:
			case <-time.After(time.Millisecond * 10):
				// Assert no blocking when template expanding.
				require.Fail(t, "unexpected blocking when template expanding.")
			}
		}
		return EngineQueryFunc(suite.QueryEngine(), suite.Storage())(ctx, q, ts)
	}
	go func() {
		<-startQueryCh
		_ = ruleWithQueryInTemplate.Health()
		_ = ruleWithQueryInTemplate.LastError()
		_ = ruleWithQueryInTemplate.GetEvaluationDuration()
		_ = ruleWithQueryInTemplate.GetEvaluationTimestamp()
		close(getDoneCh)
	}()
	_, err = ruleWithQueryInTemplate.Eval(
		suite.Context(), evalTime, slowQueryFunc, nil, 0,
	)
	require.NoError(t, err)
}

func BenchmarkAlertingRuleAtomicField(b *testing.B) {
	b.ReportAllocs()
	rule := NewAlertingRule("bench", nil, 0, nil, nil, nil, "", true, nil)
	done := make(chan struct{})
	go func() {
		for i := 0; i < b.N; i++ {
			rule.GetEvaluationTimestamp()
		}
		close(done)
	}()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rule.SetEvaluationTimestamp(time.Now())
		}
	})
	<-done
}

func TestAlertingRuleDuplicate(t *testing.T) {
	storage := teststorage.New(t)
	defer storage.Close()

	opts := promql.EngineOpts{
		Logger:     nil,
		Reg:        nil,
		MaxSamples: 10,
		Timeout:    10 * time.Second,
	}

	engine := promql.NewEngine(opts)
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	now := time.Now()

	expr, _ := parser.ParseExpr(`vector(0) or label_replace(vector(0),"test","x","","")`)
	rule := NewAlertingRule(
		"foo",
		expr,
		time.Minute,
		labels.FromStrings("test", "test"),
		nil,
		nil,
		"",
		true, log.NewNopLogger(),
	)
	_, err := rule.Eval(ctx, now, EngineQueryFunc(engine, storage), nil, 0)
	require.Error(t, err)
	require.EqualError(t, err, "vector contains metrics with the same labelset after applying alert labels")
}

func TestAlertingRuleLimit(t *testing.T) {
	suite, err := promql.NewTest(t, `
		load 1m
			metric{label="1"} 1
			metric{label="2"} 1
	`)
	require.NoError(t, err)
	defer suite.Close()

	require.NoError(t, suite.Run())

	tests := []struct {
		limit int
		err   string
	}{
		{
			limit: 0,
		},
		{
			limit: -1,
		},
		{
			limit: 2,
		},
		{
			limit: 1,
			err:   "exceeded limit of 1 with 2 alerts",
		},
	}

	expr, _ := parser.ParseExpr(`metric > 0`)
	rule := NewAlertingRule(
		"foo",
		expr,
		time.Minute,
		labels.FromStrings("test", "test"),
		nil,
		nil,
		"",
		true, log.NewNopLogger(),
	)

	evalTime := time.Unix(0, 0)

	for _, test := range tests {
		_, err := rule.Eval(suite.Context(), evalTime, EngineQueryFunc(suite.QueryEngine(), suite.Storage()), nil, test.limit)
		if err != nil {
			require.EqualError(t, err, test.err)
		} else if test.err != "" {
			t.Errorf("Expected errror %s, got none", test.err)
		}
	}
}

func TestQueryForStateSeries(t *testing.T) {
	testError := errors.New("test error")

	type testInput struct {
		selectMockFunction func(sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet
		expectedSeries     storage.Series
		expectedError      error
	}

	tests := []testInput{
		// Test for empty series.
		{
			selectMockFunction: func(sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
				return storage.EmptySeriesSet()
			},
			expectedSeries: nil,
			expectedError:  nil,
		},
		// Test for error series.
		{
			selectMockFunction: func(sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
				return storage.ErrSeriesSet(testError)
			},
			expectedSeries: nil,
			expectedError:  testError,
		},
		// Test for mock series.
		{
			selectMockFunction: func(sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
				return storage.TestSeriesSet(storage.MockSeries(
					[]int64{1, 2, 3},
					[]float64{1, 2, 3},
					[]string{"__name__", "ALERTS_FOR_STATE", "alertname", "TestRule", "severity", "critical"},
				))
			},
			expectedSeries: storage.MockSeries(
				[]int64{1, 2, 3},
				[]float64{1, 2, 3},
				[]string{"__name__", "ALERTS_FOR_STATE", "alertname", "TestRule", "severity", "critical"},
			),
			expectedError: nil,
		},
	}

	testFunc := func(tst testInput) {
		querier := &storage.MockQuerier{
			SelectMockFunction: tst.selectMockFunction,
		}

		rule := NewAlertingRule(
			"TestRule",
			nil,
			time.Minute,
			labels.FromStrings("severity", "critical"),
			nil, nil, "", true, nil,
		)

		alert := &Alert{
			State:       0,
			Labels:      nil,
			Annotations: nil,
			Value:       0,
			ActiveAt:    time.Time{},
			FiredAt:     time.Time{},
			ResolvedAt:  time.Time{},
			LastSentAt:  time.Time{},
			ValidUntil:  time.Time{},
		}

		series, err := rule.QueryforStateSeries(alert, querier)

		require.Equal(t, tst.expectedSeries, series)
		require.Equal(t, tst.expectedError, err)
	}

	for _, tst := range tests {
		testFunc(tst)
	}
}
