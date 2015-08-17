// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.  See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Tyler Neely (t@jujit.su)

// IMPORTANT: only subscribe to the metric stream
// using buffered channels that are regularly
// flushed, as reaper will NOT block while trying
// to send metrics to a subscriber, and will ignore
// a subscriber if they fail to clear their channel
// 3 times in a row!

package loghisto

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
)

const (
	// precision effects the bucketing used during histogram value compression.
	precision = 100
)

// ProcessedMetricSet contains human-readable metrics that may also be
// suitable for storage in time-series databases.
type ProcessedMetricSet struct {
	Time    time.Time
	Metrics map[string]float64
}

// RawMetricSet contains metrics in a form that supports generation of
// percentiles and other rich statistics.
type RawMetricSet struct {
	Time       time.Time
	Counters   map[string]uint64
	Rates      map[string]uint64
	Histograms map[string]map[int16]*uint64
	Gauges     map[string]float64
}

// TimerToken facilitates concurrent timings of durations of the same label.
type TimerToken struct {
	Name         string
	Start        time.Time
	MetricSystem *MetricSystem
}

// proportion is a compact value with a corresponding count of
// occurrences in this interval.
type proportion struct {
	Value float64
	Count uint64
}

// proportionArray is a sortable collection of proportion types.
type proportionArray []proportion

// MetricSystem facilitates the collection and distribution of metrics.
type MetricSystem struct {
	// percentiles is a mapping from labels to desired percentiles to be
	// calculated by the MetricSystem
	percentiles map[string]float64
	// interval is the duration between collections and broadcasts of metrics
	// to subscribers.
	interval time.Duration
	// subscribeToRawMetrics allows subscription to a RawMetricSet generated
	// by reaper at the end of each interval on a sent channel.
	subscribeToRawMetrics chan chan *RawMetricSet
	// unsubscribeFromRawMetrics allows subscribers to unsubscribe from
	// receiving a RawMetricSet on the sent channel.
	unsubscribeFromRawMetrics chan chan *RawMetricSet
	// subscribeToProcessedMetrics allows subscription to a ProcessedMetricSet
	// generated by reaper at the end of each interval on a sent channel.
	subscribeToProcessedMetrics chan chan *ProcessedMetricSet
	// unsubscribeFromProcessedMetrics allows subscribers to unsubscribe from
	// receiving a ProcessedMetricSet on the sent channel.
	unsubscribeFromProcessedMetrics chan chan *ProcessedMetricSet
	// rawSubscribers stores current subscribers to RawMetrics
	rawSubscribers map[chan *RawMetricSet]struct{}
	// rawBadSubscribers tracks misbehaving subscribers who do not clear their
	// subscription channels regularly.
	rawBadSubscribers map[chan *RawMetricSet]int
	// processedSubscribers stores current subscribers to ProcessedMetrics
	processedSubscribers map[chan *ProcessedMetricSet]struct{}
	// processedBadSubscribers tracks misbehaving subscribers who do not clear
	// their subscription channels regularly.
	processedBadSubscribers map[chan *ProcessedMetricSet]int
	// subscribersMu controls access to subscription structures
	subscribersMu sync.RWMutex
	// counterStore maintains the total counts of counters.
	counterStore   map[string]*uint64
	counterStoreMu sync.RWMutex
	// counterCache aggregates new Counters until they are collected by reaper().
	counterCache map[string]*uint64
	// counterMu controls access to counterCache.
	counterMu sync.RWMutex
	// histogramCache aggregates Histograms until they are collected by reaper().
	histogramCache map[string]map[int16]*uint64
	// histogramMu controls access to histogramCache.
	histogramMu sync.RWMutex
	// histogramCountStore keeps track of aggregate counts and sums for aggregate
	// mean calculation.
	histogramCountStore map[string]*uint64
	// histogramCountMu controls access to the histogramCountStore.
	histogramCountMu sync.RWMutex
	// gaugeFuncs maps metrics to functions used for calculating their value
	gaugeFuncs map[string]func() float64
	// gaugeFuncsMu controls access to the gaugeFuncs map.
	gaugeFuncsMu sync.Mutex
	// Has reaper() been started?
	reaping bool
	// Close this to bring down this MetricSystem
	shutdownChan chan struct{}
}

// Metrics is the default metric system, which collects and broadcasts metrics
// to subscribers once every 60 seconds.  Also includes default system stats.
var Metrics = NewMetricSystem(60*time.Second, true)

// NewMetricSystem returns a new metric system that collects and broadcasts
// metrics after each interval.
func NewMetricSystem(interval time.Duration, sysStats bool) *MetricSystem {
	ms := &MetricSystem{
		percentiles: map[string]float64{
			"%s_min":   0,
			"%s_50":    .5,
			"%s_75":    .75,
			"%s_90":    .9,
			"%s_95":    .95,
			"%s_99":    .99,
			"%s_99.9":  .999,
			"%s_99.99": .9999,
			"%s_max":   1,
		},
		interval:                        interval,
		subscribeToRawMetrics:           make(chan chan *RawMetricSet, 64),
		unsubscribeFromRawMetrics:       make(chan chan *RawMetricSet, 64),
		subscribeToProcessedMetrics:     make(chan chan *ProcessedMetricSet, 64),
		unsubscribeFromProcessedMetrics: make(chan chan *ProcessedMetricSet, 64),
		rawSubscribers:                  make(map[chan *RawMetricSet]struct{}),
		rawBadSubscribers:               make(map[chan *RawMetricSet]int),
		processedSubscribers:            make(map[chan *ProcessedMetricSet]struct{}),
		processedBadSubscribers:         make(map[chan *ProcessedMetricSet]int),
		counterStore:                    make(map[string]*uint64),
		counterCache:                    make(map[string]*uint64),
		histogramCache:                  make(map[string]map[int16]*uint64),
		histogramCountStore:             make(map[string]*uint64),
		gaugeFuncs:                      make(map[string]func() float64),
		shutdownChan:                    make(chan struct{}),
	}
	if sysStats {
		ms.gaugeFuncsMu.Lock()
		ms.gaugeFuncs["sys.Alloc"] = func() float64 {
			memStats := new(runtime.MemStats)
			runtime.ReadMemStats(memStats)
			return float64(memStats.Alloc)
		}
		ms.gaugeFuncs["sys.NumGC"] = func() float64 {
			memStats := new(runtime.MemStats)
			runtime.ReadMemStats(memStats)
			return float64(memStats.NumGC)
		}
		ms.gaugeFuncs["sys.PauseTotalNs"] = func() float64 {
			memStats := new(runtime.MemStats)
			runtime.ReadMemStats(memStats)
			return float64(memStats.PauseTotalNs)
		}
		ms.gaugeFuncs["sys.NumGoroutine"] = func() float64 {
			return float64(runtime.NumGoroutine())
		}
		ms.gaugeFuncsMu.Unlock()
	}
	return ms
}

// SpecifyPercentiles allows users to override the default collected
// and reported percentiles.
func (ms *MetricSystem) SpecifyPercentiles(percentiles map[string]float64) {
	ms.percentiles = percentiles
}

// SubscribeToRawMetrics registers a channel to receive RawMetricSets
// periodically generated by reaper at each interval.
func (ms *MetricSystem) SubscribeToRawMetrics(metricStream chan *RawMetricSet) {
	ms.subscribeToRawMetrics <- metricStream
}

// UnsubscribeFromRawMetrics registers a channel to receive RawMetricSets
// periodically generated by reaper at each interval.
func (ms *MetricSystem) UnsubscribeFromRawMetrics(
	metricStream chan *RawMetricSet) {
	ms.unsubscribeFromRawMetrics <- metricStream
}

// SubscribeToProcessedMetrics registers a channel to receive
// ProcessedMetricSets periodically generated by reaper at each interval.
func (ms *MetricSystem) SubscribeToProcessedMetrics(
	metricStream chan *ProcessedMetricSet) {
	ms.subscribeToProcessedMetrics <- metricStream
}

// UnsubscribeFromProcessedMetrics registers a channel to receive
// ProcessedMetricSets periodically generated by reaper at each interval.
func (ms *MetricSystem) UnsubscribeFromProcessedMetrics(
	metricStream chan *ProcessedMetricSet) {
	ms.unsubscribeFromProcessedMetrics <- metricStream
}

// StartTimer begins a timer and returns a token which is required for halting
// the timer.  This allows for concurrent timings under the same name.
func (ms *MetricSystem) StartTimer(name string) TimerToken {
	return TimerToken{
		Name:         name,
		Start:        time.Now(),
		MetricSystem: ms,
	}
}

// Stop stops a timer given by StartTimer, submits a Histogram of its duration
// in nanoseconds, and returns its duration in nanoseconds.
func (tt *TimerToken) Stop() time.Duration {
	duration := time.Since(tt.Start)
	tt.MetricSystem.Histogram(tt.Name, float64(duration.Nanoseconds()))
	return duration
}

// Counter is used for recording a running count of the total occurrences of
// a particular event.  A rate is also exported for the amount that a counter
// has increased during an interval of this MetricSystem.
func (ms *MetricSystem) Counter(name string, amount uint64) {
	ms.counterMu.RLock()
	_, exists := ms.counterCache[name]
	// perform lock promotion when we need more control
	if !exists {
		ms.counterMu.RUnlock()
		ms.counterMu.Lock()
		_, syncExists := ms.counterCache[name]
		if !syncExists {
			var z uint64
			ms.counterCache[name] = &z
		}
		ms.counterMu.Unlock()
		ms.counterMu.RLock()
	}
	atomic.AddUint64(ms.counterCache[name], amount)
	ms.counterMu.RUnlock()
}

// Histogram is used for generating rich metrics, such as percentiles, from
// periodically occurring continuous values.
func (ms *MetricSystem) Histogram(name string, value float64) {
	compressedValue := compress(value)
	ms.histogramMu.RLock()
	_, present := ms.histogramCache[name][compressedValue]
	if !present {
		ms.histogramMu.RUnlock()
		ms.histogramMu.Lock()
		_, syncPresent := ms.histogramCache[name][compressedValue]
		if !syncPresent {
			var z uint64
			_, mapPresent := ms.histogramCache[name]
			if !mapPresent {
				ms.histogramCache[name] = make(map[int16]*uint64)
			}
			ms.histogramCache[name][compressedValue] = &z
		}
		ms.histogramMu.Unlock()
		ms.histogramMu.RLock()
	}
	atomic.AddUint64(ms.histogramCache[name][compressedValue], 1)
	ms.histogramMu.RUnlock()
}

// RegisterGaugeFunc registers a function to be called at each interval
// whose return value will be used to populate the <name> metric.
func (ms *MetricSystem) RegisterGaugeFunc(name string, f func() float64) {
	ms.gaugeFuncsMu.Lock()
	ms.gaugeFuncs[name] = f
	ms.gaugeFuncsMu.Unlock()
}

// DeregisterGaugeFunc deregisters a function for the <name> metric.
func (ms *MetricSystem) DeregisterGaugeFunc(name string) {
	ms.gaugeFuncsMu.Lock()
	delete(ms.gaugeFuncs, name)
	ms.gaugeFuncsMu.Unlock()
}

// compress takes a float64 and lossily shrinks it to an int16 to facilitate
// bucketing of histogram values, staying within 1% of the true value.  This
// fails for large values of 1e142 and above, and is inaccurate for values
// closer to 0 than +/- 0.51 or +/- math.Inf.
func compress(value float64) int16 {
	i := int16(precision*math.Log(1.0+math.Abs(value)) + 0.5)
	if value < 0 {
		return -1 * i
	}
	return i
}

// decompress takes a lossily shrunk int16 and returns a float64 within 1% of
// the original float64 passed to compress.
func decompress(compressedValue int16) float64 {
	f := math.Exp(math.Abs(float64(compressedValue))/precision) - 1.0
	if compressedValue < 0 {
		return -1.0 * f
	}
	return f
}

// processHistograms derives rich metrics from histograms, currently
// percentiles, sum, count, and mean.
func (ms *MetricSystem) processHistograms(name string,
	valuesToCounts map[int16]*uint64) map[string]float64 {
	output := make(map[string]float64)
	totalSum := float64(0)
	totalCount := uint64(0)
	proportions := make([]proportion, 0, len(valuesToCounts))
	for compressedValue, count := range valuesToCounts {
		value := decompress(compressedValue)
		totalSum += value * float64(*count)
		totalCount += *count
		proportions = append(proportions, proportion{Value: value, Count: *count})
	}

	sumName := fmt.Sprintf("%s_sum", name)
	countName := fmt.Sprintf("%s_count", name)
	avgName := fmt.Sprintf("%s_avg", name)

	// increment interval sum and count
	output[countName] = float64(totalCount)
	output[sumName] = totalSum
	output[avgName] = totalSum / float64(totalCount)

	// increment aggregate sum and count
	ms.histogramCountMu.RLock()
	_, present := ms.histogramCountStore[sumName]
	if !present {
		ms.histogramCountMu.RUnlock()
		ms.histogramCountMu.Lock()
		_, syncPresent := ms.histogramCountStore[sumName]
		if !syncPresent {
			var x uint64
			ms.histogramCountStore[sumName] = &x
			var z uint64
			ms.histogramCountStore[countName] = &z
		}
		ms.histogramCountMu.Unlock()
		ms.histogramCountMu.RLock()
	}
	atomic.AddUint64(ms.histogramCountStore[sumName], uint64(totalSum))
	atomic.AddUint64(ms.histogramCountStore[countName], totalCount)
	ms.histogramCountMu.RUnlock()

	for label, p := range ms.percentiles {
		value, err := percentile(totalCount, proportions, p)
		if err != nil {
			glog.Errorf("unable to calculate percentile: %s", err)
		} else {
			output[fmt.Sprintf(label, name)] = value
		}
	}
	return output
}

// These next 3 methods are for the implementation of sort.Interface

func (s proportionArray) Len() int {
	return len(s)
}

func (s proportionArray) Less(i, j int) bool {
	return s[i].Value < s[j].Value
}

func (s proportionArray) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// percentile calculates a percentile represented as a float64 between 0 and 1
// inclusive from a proportionArray.  totalCount is the sum of all counts of
// elements in the proportionArray.
func percentile(totalCount uint64, proportions proportionArray,
	percentile float64) (float64, error) {
	//TODO(tyler) handle multiple percentiles at once for efficiency
	sort.Sort(proportions)
	sofar := uint64(0)
	for _, proportion := range proportions {
		sofar += proportion.Count
		if float64(sofar)/float64(totalCount) >= percentile {
			return proportion.Value, nil
		}
	}
	return 0, errors.New("Invalid percentile.  Should be between 0 and 1.")
}

func (ms *MetricSystem) collectRawMetrics() *RawMetricSet {
	normalizedInterval := time.Unix(0, time.Now().UnixNano()/
		ms.interval.Nanoseconds()*
		ms.interval.Nanoseconds())

	ms.counterMu.Lock()
	freshCounters := ms.counterCache
	ms.counterCache = make(map[string]*uint64)
	ms.counterMu.Unlock()

	rates := make(map[string]uint64)
	for name, count := range freshCounters {
		rates[name] = *count
	}

	counters := make(map[string]uint64)
	ms.counterStoreMu.RLock()
	// update counters
	for name, count := range freshCounters {
		_, exists := ms.counterStore[name]
		// only take a write lock when it's a totally new counter
		if !exists {
			ms.counterStoreMu.RUnlock()
			ms.counterStoreMu.Lock()
			_, syncExists := ms.counterStore[name]
			if !syncExists {
				var z uint64
				ms.counterStore[name] = &z
			}
			ms.counterStoreMu.Unlock()
			ms.counterStoreMu.RLock()
		}
		atomic.AddUint64(ms.counterStore[name], *count)
	}
	// copy counters for export
	for name, count := range ms.counterStore {
		counters[name] = *count
	}
	ms.counterStoreMu.RUnlock()

	ms.histogramMu.Lock()
	histograms := ms.histogramCache
	ms.histogramCache = make(map[string]map[int16]*uint64)
	ms.histogramMu.Unlock()

	ms.gaugeFuncsMu.Lock()
	gauges := make(map[string]float64)
	for name, f := range ms.gaugeFuncs {
		gauges[name] = f()
	}
	ms.gaugeFuncsMu.Unlock()

	return &RawMetricSet{
		Time:       normalizedInterval,
		Counters:   counters,
		Rates:      rates,
		Histograms: histograms,
		Gauges:     gauges,
	}
}

// processMetrics (potentially slowly) creates human consumable metrics from a
// RawMetricSet, deriving rich statistics from histograms such as percentiles.
func (ms *MetricSystem) processMetrics(
	rawMetrics *RawMetricSet) *ProcessedMetricSet {
	metrics := make(map[string]float64)

	for name, count := range rawMetrics.Counters {
		metrics[name] = float64(count)
	}

	for name, count := range rawMetrics.Rates {
		metrics[fmt.Sprintf("%s_rate", name)] = float64(count)
	}

	for name, valuesToCounts := range rawMetrics.Histograms {
		for histoName, histoValue := range ms.processHistograms(name, valuesToCounts) {
			metrics[histoName] = histoValue
		}
	}

	for name, value := range rawMetrics.Gauges {
		metrics[name] = value
	}

	return &ProcessedMetricSet{Time: rawMetrics.Time, Metrics: metrics}
}

func (ms *MetricSystem) updateSubscribers() {
	for {
		select {
		case subscriber := <-ms.subscribeToRawMetrics:
			ms.rawSubscribers[subscriber] = struct{}{}
		case unsubscriber := <-ms.unsubscribeFromRawMetrics:
			delete(ms.rawSubscribers, unsubscriber)
		case subscriber := <-ms.subscribeToProcessedMetrics:
			ms.processedSubscribers[subscriber] = struct{}{}
		case unsubscriber := <-ms.unsubscribeFromProcessedMetrics:
			delete(ms.processedSubscribers, unsubscriber)
		default: // no changes in subscribers
			return
		}
	}
}

// reaper wakes up every <interval> seconds,
// collects and processes metrics, and pushes
// them to the corresponding subscribing channels.
func (ms *MetricSystem) reaper() {
	ms.reaping = true

	// create goroutine pool to handle multiple processing tasks at once
	processChan := make(chan func(), 16)
	for i := 0; i < int(math.Max(float64(runtime.NumCPU()/4), 4)); i++ {
		go func() {
			for {
				c, ok := <-processChan
				if !ok {
					return
				}
				c()
			}
		}()
	}

	// begin reaper main loop
	for {
		// sleep until the next interval, or die if shutdownChan is closed
		tts := ms.interval.Nanoseconds() -
			(time.Now().UnixNano() % ms.interval.Nanoseconds())
		select {
		case <-time.After(time.Duration(tts)):
		case <-ms.shutdownChan:
			ms.reaping = false
			close(processChan)
			return
		}

		rawMetrics := ms.collectRawMetrics()

		ms.updateSubscribers()

		// broadcast raw metrics
		for subscriber := range ms.rawSubscribers {
			// new subscribers get all counters, otherwise just the new diffs
			select {
			case subscriber <- rawMetrics:
				delete(ms.rawBadSubscribers, subscriber)
			default:
				ms.rawBadSubscribers[subscriber]++
				glog.Error("a raw subscriber has allowed their channel to fill up. ",
					"dropping their metrics on the floor rather than blocking.")
				if ms.rawBadSubscribers[subscriber] >= 2 {
					glog.Error("this raw subscriber has caused dropped metrics at ",
						"least 3 times in a row.  closing the channel.")
					delete(ms.rawSubscribers, subscriber)
					close(subscriber)
				}
			}
		}

		// Perform the rest in another goroutine since processing is not
		// gauranteed to complete before the interval is up.
		sendProcessed := func() {
			// this is potentially expensive if there is a massive number of metrics
			processedMetrics := ms.processMetrics(rawMetrics)

			// add aggregate mean
			for name := range rawMetrics.Histograms {
				ms.histogramCountMu.RLock()
				aggCountPtr, countPresent :=
					ms.histogramCountStore[fmt.Sprintf("%s_count", name)]
				aggCount := atomic.LoadUint64(aggCountPtr)
				aggSumPtr, sumPresent :=
					ms.histogramCountStore[fmt.Sprintf("%s_sum", name)]
				aggSum := atomic.LoadUint64(aggSumPtr)
				ms.histogramCountMu.RUnlock()

				if countPresent && sumPresent && aggCount > 0 {
					processedMetrics.Metrics[fmt.Sprintf("%s_agg_avg", name)] =
						float64(aggSum / aggCount)
					processedMetrics.Metrics[fmt.Sprintf("%s_agg_count", name)] =
						float64(aggCount)
					processedMetrics.Metrics[fmt.Sprintf("%s_agg_sum", name)] =
						float64(aggSum)
				}
			}

			// broadcast processed metrics
			for subscriber := range ms.processedSubscribers {
				select {
				case subscriber <- processedMetrics:
					ms.subscribersMu.Lock()
					delete(ms.processedBadSubscribers, subscriber)
					ms.subscribersMu.Unlock()
				default:
					ms.processedBadSubscribers[subscriber]++
					glog.Error("a subscriber has allowed their channel to fill up. ",
						"dropping their metrics on the floor rather than blocking.")
					if ms.processedBadSubscribers[subscriber] >= 2 {
						glog.Error("this subscriber has caused dropped metrics at ",
							"least 3 times in a row.  closing the channel.")
						ms.subscribersMu.Lock()
						delete(ms.processedSubscribers, subscriber)
						ms.subscribersMu.Unlock()
						close(subscriber)
					}
				}
			}
		}
		select {
		case processChan <- sendProcessed:
		default:
			// processChan has filled up, this metric load is not sustainable
			glog.Errorf("processing of metrics is taking longer than this node can "+
				"handle.  dropping this entire interval of %s metrics on the "+
				"floor rather than blocking the reaper.", rawMetrics.Time)
		}
	} // end main reaper loop
}

// Start spawns a goroutine for merging metrics into caches from
// metric submitters, and a reaper goroutine that harvests metrics at the
// default interval of every 60 seconds.
func (ms *MetricSystem) Start() {
	if !ms.reaping {
		go ms.reaper()
	}
}

// Stop shuts down a MetricSystem
func (ms *MetricSystem) Stop() {
	close(ms.shutdownChan)
}
