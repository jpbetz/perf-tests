/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
TODO(krzysied): This measurement should replace api_responsiveness.go.
*/

package slos

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/perf-tests/clusterloader2/pkg/errors"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	measurementutil "k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	apiResponsivenessPrometheusMeasurementName = "APIResponsivenessPrometheus"

	// latencyQuery %v should be replaced with query window size.
	latencyQuery = "quantile_over_time(0.99, apiserver:apiserver_request_latency:histogram_quantile[%v])"
	// countQuery %v should be replaced with query window size.
	countQuery = "sum(increase(apiserver_request_latency_seconds_count[%v])) by (resource, subresource, scope, verb)"

	latencyWindowSize = 5 * time.Minute
	queryTimeout      = 5 * time.Minute
	queryInterval     = 30 * time.Second
)

func init() {
	measurement.Register(apiResponsivenessPrometheusMeasurementName, createAPIResponsivenessPrometheusMeasurement)
}

func createAPIResponsivenessPrometheusMeasurement() measurement.Measurement {
	return &apiResponsivenessMeasurementPrometheus{}
}

type apiResponsivenessMeasurementPrometheus struct {
	startTime time.Time
	apiCalls  map[string]*apiCall
}

func (a *apiResponsivenessMeasurementPrometheus) Execute(config *measurement.MeasurementConfig) ([]measurement.Summary, error) {
	var summaries []measurement.Summary

	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return summaries, err
	}

	switch action {
	case "start":
		a.start()
	case "gather":
		summary, err := a.gather(config.PrometheusFramework.GetClientSets().GetClient())
		if err == nil || errors.IsMetricViolationError(err) {
			summaries = append(summaries, summary)
		}
		return summaries, err
	default:
		return summaries, fmt.Errorf("unknown action %v", action)
	}

	return summaries, nil
}

// Dispose cleans up after the measurement.
func (a *apiResponsivenessMeasurementPrometheus) Dispose() {}

// String returns string representation of this measurement.
func (*apiResponsivenessMeasurementPrometheus) String() string {
	return apiResponsivenessPrometheusMeasurementName
}

func (a *apiResponsivenessMeasurementPrometheus) start() {
	a.startTime = time.Now()
}

func (a *apiResponsivenessMeasurementPrometheus) gather(c clientset.Interface) (measurement.Summary, error) {
	apiCalls, err := a.gatherApiCalls(c)
	if err != nil {
		klog.Errorf("%s: samples gathering error: %v", a, err)
	}

	metrics := &apiResponsiveness{ApiCalls: apiCalls}
	sort.Sort(sort.Reverse(metrics))
	var badMetrics []string
	top := 5
	for i := range metrics.ApiCalls {
		isBad := false
		latencyThreshold := getLatencyThreshold(&metrics.ApiCalls[i])
		if metrics.ApiCalls[i].Latency.Perc99 > latencyThreshold {
			isBad = true
			badMetrics = append(badMetrics, fmt.Sprintf("got: %+v; expected perc99 <= %v", metrics.ApiCalls[i], latencyThreshold))
		}
		if top > 0 || isBad {
			top--
			prefix := ""
			if isBad {
				prefix = "WARNING "
			}
			klog.Infof("%s: %vTop latency metric: %+v; threshold: %v", a, prefix, metrics.ApiCalls[i], latencyThreshold)
		}
	}
	if len(badMetrics) > 0 {
		return metrics, errors.NewMetricViolationError("top latency metric", fmt.Sprintf("there should be no high-latency requests, but: %v", badMetrics))
	}
	return metrics, nil
}

func (a *apiResponsivenessMeasurementPrometheus) gatherApiCalls(c clientset.Interface) ([]apiCall, error) {
	measurementEnd := time.Now()
	measurementDuration := measurementEnd.Sub(a.startTime)
	// Latency measurement is based on 5m window aggregation,
	// therefore first 5 minutes of the test should be skipped.
	latencymeasurementDuration := measurementDuration - latencyWindowSize
	if latencymeasurementDuration < time.Minute {
		latencymeasurementDuration = time.Minute
	}
	timeBoundedLatencyQuery := fmt.Sprintf(latencyQuery, measurementutil.ToPrometheusTime(latencymeasurementDuration))
	latencySamples, err := gatherSamples(c, timeBoundedLatencyQuery, measurementEnd)
	if err != nil {
		return nil, err
	}
	timeBoundedCountQuery := fmt.Sprintf(countQuery, measurementutil.ToPrometheusTime(measurementDuration))
	countSamples, err := gatherSamples(c, timeBoundedCountQuery, measurementEnd)
	if err != nil {
		return nil, err
	}
	return a.convertToApiCalls(latencySamples, countSamples)
}

func (a *apiResponsivenessMeasurementPrometheus) convertToApiCalls(latencySamples, countSamples []*model.Sample) ([]apiCall, error) {
	apiCalls := make(map[string]*apiCall)
	ignoredResources := sets.NewString("events")
	// TODO(krzysied): figure out why we're getting non-capitalized proxy and fix this.
	ignoredVerbs := sets.NewString("WATCH", "WATCHLIST", "PROXY", "proxy", "CONNECT")

	for _, sample := range latencySamples {
		resource := string(sample.Metric["resource"])
		subresource := string(sample.Metric["subresource"])
		verb := string(sample.Metric["verb"])
		scope := string(sample.Metric["scope"])
		quantile, err := strconv.ParseFloat(string(sample.Metric["quantile"]), 64)
		if err != nil {
			return nil, err
		}
		if ignoredResources.Has(resource) || ignoredVerbs.Has(verb) {
			continue
		}

		latency := time.Duration(float64(sample.Value) * float64(time.Second))
		addLatency(apiCalls, resource, subresource, verb, scope, quantile, latency)
	}

	for _, sample := range countSamples {
		resource := string(sample.Metric["resource"])
		subresource := string(sample.Metric["subresource"])
		verb := string(sample.Metric["verb"])
		scope := string(sample.Metric["scope"])
		if ignoredResources.Has(resource) || ignoredVerbs.Has(verb) {
			continue
		}

		count := int(math.Round(float64(sample.Value)))
		addCount(apiCalls, resource, subresource, verb, scope, count)
	}

	var result []apiCall
	for _, call := range apiCalls {
		result = append(result, *call)
	}
	return result, nil
}

func getApiCall(apiCalls map[string]*apiCall, resource, subresource, verb, scope string) *apiCall {
	key := getMetricKey(resource, subresource, verb, scope)
	call, exists := apiCalls[key]
	if !exists {
		call = &apiCall{
			Resource:    resource,
			Subresource: subresource,
			Verb:        verb,
			Scope:       scope,
		}
		apiCalls[key] = call
	}
	return call
}

func addLatency(apiCalls map[string]*apiCall, resource, subresource, verb, scope string, quantile float64, latency time.Duration) {
	call := getApiCall(apiCalls, resource, subresource, verb, scope)
	call.Latency.SetQuantile(quantile, latency)
}

func addCount(apiCalls map[string]*apiCall, resource, subresource, verb, scope string, count int) {
	if count == 0 {
		return
	}
	call := getApiCall(apiCalls, resource, subresource, verb, scope)
	call.Count = count
}

func gatherSamples(c clientset.Interface, query string, queryTime time.Time) ([]*model.Sample, error) {
	if queryTime.IsZero() {
		return nil, fmt.Errorf("query time can't be zero")
	}

	var body []byte
	var queryErr error
	params := map[string]string{
		"query": query,
		"time":  queryTime.Format(time.RFC3339),
	}
	if err := wait.PollImmediate(queryInterval, queryTimeout, func() (bool, error) {
		body, queryErr = c.CoreV1().
			Services("monitoring").
			ProxyGet("http", "prometheus-k8s", "9090", "api/v1/query", params).
			DoRaw()
		if queryErr != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		if queryErr != nil {
			return nil, fmt.Errorf("query error: %v", queryErr)
		}
		return nil, fmt.Errorf("query error: %v", err)
	}

	samples, err := measurementutil.ExtractMetricSamples2(body)
	if err != nil {
		return nil, fmt.Errorf("exctracting error: %v", err)
	}

	var resultSamples []*model.Sample
	for _, sample := range samples {
		if !math.IsNaN(float64(sample.Value)) {
			resultSamples = append(resultSamples, sample)
		}
	}
	return resultSamples, nil
}

func getMetricKey(resource, subresource, verb, scope string) string {
	return fmt.Sprintf("%s|%s|%s|%s", resource, subresource, verb, scope)
}

func getLatencyThreshold(call *apiCall) time.Duration {
	isListCall := (call.Verb == "LIST")
	isClusterScopedCall := (call.Scope == "cluster")
	latencyThreshold := apiCallLatencyThreshold
	if isListCall {
		latencyThreshold = apiListCallLatencyThreshold
		if isClusterScopedCall {
			latencyThreshold = apiClusterScopeListCallThreshold
		}
	}
	return latencyThreshold
}
