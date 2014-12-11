loghisto
============
A metric system for high performance counters and histograms.  Unlike popular metric systems today, this does not destroy the accuracy of histograms by sampling.  Instead, a logarithmic bucketing function compresses values, generally within 1% of their true value (although between 0 and 1 the precision loss may not be within this boundary).  This allows for extreme compression, which allows us to calculate arbitrarily high percentiles with no loss of accuracy - just a small amount of precision.  This is particularly useful for highly-clustered events that are tolerant of a small precision loss, but for which you REALLY care about what the tail looks like, such as measuring latency across a distributed system.

Copied out of my work for the CockroachDB metrics system.  Based on an algorithm created by Keith Frost.


### running a print benchmark for quick analysis
```go
package main

import (
  "runtime"
  "github.com/spacejam/loghisto"
)

func benchmark() {
  // do some stuff
}

func main() {
  numCPU := runtime.NumCPU()
  runtime.GOMAXPROCS(numCPU)

  desiredConcurrency := uint(100)
  loghisto.PrintBenchmark("benchmark1234", desiredConcurrency, benchmark)
}
```
results in something like this printed to stdout each second:
```
2014-12-11 14:59:30 -0500 EST
benchmark1234_count:     1.401589e+06
benchmark1234_max:       646933.2852939675
benchmark1234_99.99:     1001.2472422902518
benchmark1234_99.9:      1001.2472422902518
benchmark1234_99:        1001.2472422902518
benchmark1234_50:        61.177922934760794
benchmark1234_min:       0
benchmark1234_sum:       1.1099876067243168e+08
benchmark1234_avg:       79.19494279166837
benchmark1234_agg_avg:   79
benchmark1234_agg_count: 1.401589e+06
benchmark1234_agg_sum:   1.1099876e+08
sys.Alloc:               839952
sys.NumGC:               156
sys.PauseTotalNs:        2.6633438e+07
sys.NumGoroutine:        113
```
### adding an embedded metric system to your code
```go
import (
  "time"
  "fmt"
  "github.com/spacejam/loghisto"
)
func ExampleMetricSystem() {
  // Create metric system that reports once a minute, and includes stats
  // about goroutines, memory usage and GC.
  includeGoProcessStats := true
  ms := loghisto.NewMetricSystem(time.Minute, includeGoProcessStats)
  ms.Start()

  // create a channel that subscribes to metrics as they are produced once 
  // per minute.
  // NOTE: if you allow this channel to fill up, the metric system will NOT
  // block, and  will FORGET about your channel if you fail to unblock the
  // channel after 3 configured intervals (in this case 3 minutes) rather
  // than causing a memory leak.
  myMetricStream := make(chan *loghisto.ProcessedMetricSet, 2)
  ms.SubscribeToProcessedMetrics(myMetricStream)

  // create some metrics
  timeToken := ms.StartTimer("time for creating a counter and histo")
  ms.Counter("some event", 1)
  ms.Histogram("some measured thing", 123)
  timeToken.Stop()

  for m := range myMetricStream {
    fmt.Printf("number of goroutines: %f\n", m.Metrics["sys.NumGoroutine"])
  }

  // if you want to manually unsubscribe from the metric stream
  ms.UnsubscribeFromProcessedMetrics(myMetricStream)

  // to stop and clean up your metric system
  ms.Stop()
}
```
### automatically sending your metrics to OpenTSDB, KairosDB or Graphite
```go
func ExampleExternalSubmitter() {
  includeGoProcessStats := true
  ms := NewMetricSystem(time.Minute, includeGoProcessStats)
  ms.Start()
  // graphite
  s := NewSubmitter(ms, GraphiteProtocol, "tcp", "localhost:7777")
  s.Start()

  // opentsdb / kairosdb
  s := NewSubmitter(ms, OpenTSDBProtocol, "tcp", "localhost:7777")
  s.Start()

  // to tear down:
  s.Shutdown()
}
```

See code for the Graphite/OpenTSDB protocols for adding your own output plugins, it's pretty simple.
