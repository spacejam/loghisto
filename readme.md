loghisto
============
This library provides a high performance, counter and lossless accuracy histogram metric system.  It's good for collecting latency statistics in massive systems.  It doesn't throw away metrics like your standard reservoir sampling histogram metric libraries do, but instead uses a logarithmic bucketing algorithm to sacrifice a small amount of precision (generally less than 1%, but it's bad for numbers between 0 and 1).  This in turn allows you to drill into the 99.99th percentile, and you know it's within 1% of the true 99.99th percentile of the stream.  This is useful when examining your long-tail latency - something that you should not take lightly when running large scale distributed systems.
Copied out of my work for the CockroachDB metrics system.  Based on an algorithm created by Keith Frost.


### running
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
