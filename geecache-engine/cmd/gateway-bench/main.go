package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/gatewayperf"
	"log"
	"os"
	"time"
)

func main() {
	config := flag.String("config", "", "JSON workload configuration")
	out := flag.String("out", "", "artifact directory")
	flag.Parse()
	file, e := os.Open(*config)
	if e != nil {
		log.Fatal("configuration open failed")
	}
	var c gatewayperf.Config
	e = json.NewDecoder(file).Decode(&c)
	_ = file.Close()
	if e != nil {
		log.Fatal("configuration decode failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, e := gatewayperf.Run(ctx, c, *out)
	if e != nil {
		log.Printf("workload failed: %v; status counts: %v", e, result.StatusCounts)
		os.Exit(1)
	}
	log.Printf("completed %d requests; errors=%d incorrect=%d", result.Attempted, result.Errors, result.Incorrect)
}
