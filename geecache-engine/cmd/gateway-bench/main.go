package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	gatewaybench "github.com/X1Kun/simpleCache/geecache-engine/internal/bench/gateway"
)

func main() {
	config := flag.String("config", "", "JSON workload configuration")
	fixture := flag.String("fixture", "", "write validated fixture SQL and exit")
	out := flag.String("out", "", "artifact directory")
	flag.Parse()
	file, e := os.Open(*config)
	if e != nil {
		log.Fatal("configuration open failed")
	}
	var c gatewaybench.Config
	e = json.NewDecoder(file).Decode(&c)
	_ = file.Close()
	if e != nil {
		log.Fatal("configuration decode failed")
	}
	if *fixture != "" {
		if err := gatewaybench.Validate(c); err != nil {
			log.Fatal(err)
		}
		file, err := os.Create(*fixture)
		if err != nil {
			log.Fatal(err)
		}
		err = gatewaybench.WriteFixture(file, c)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			log.Fatal("fixture generation failed: ", err, closeErr)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, e := gatewaybench.Run(ctx, c, *out)
	if e != nil {
		log.Printf("workload failed: %v; status counts: %v", e, result.StatusCounts)
		os.Exit(1)
	}
	log.Printf("completed %d requests; errors=%d incorrect=%d", result.Attempted, result.Errors, result.Incorrect)
}
