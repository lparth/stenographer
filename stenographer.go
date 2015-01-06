// Copyright 2014 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Binary stenographer reads packets from the given filename based on a set of
// IPs and spits them out via STDOUT as pcap data, which should be able to be
// piped into tcpdump.
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/google/stenographer/base"
	"github.com/google/stenographer/config"
	"github.com/google/stenographer/query"
	"golang.org/x/net/context"

	_ "net/http/pprof" // server debugging info in /debug/pprof/*
)

var configFilename = flag.String(
	"config",
	"/etc/stenographer/config",
	"File location to read configuration from")

// Verbose logging.
var v = base.V

const minStenotypeRuntimeForRestart = time.Minute

// ReadConfig reads in the config specified by the --config flag.
func ReadConfig() *config.Config {
	c, err := config.ReadConfigFile(*configFilename)
	if err != nil {
		log.Fatal(err.Error())
	}
	return c
}

// runStenotypeOnce runs the stenotype binary a single time, returning any
// errors associated with its running.
func runStenotypeOnce(conf *config.Config, dir *config.Directory) error {
	// Start running stenotype.
	cmd := conf.Stenotype(dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start stenotype: %v", err)
	}
	defer cmd.Process.Signal(os.Interrupt)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("stenotype wait failed: %v", err)
	}
	return fmt.Errorf("stenotype stopped")
}

// runStenotype keeps the stenotype binary running, restarting it if necessary
// but trying not to allow crash loops.
func runStenotype(conf *config.Config, dir *config.Directory) {
	for {
		start := time.Now()
		err := runStenotypeOnce(conf, dir)
		duration := time.Since(start)
		log.Printf("Stenotype ran for %v: %v", duration, err)
		if duration < minStenotypeRuntimeForRestart {
			log.Fatalf("Stenotype ran for too little time, crashing to avoid stenotype crash loop")
		}
	}
}

func main() {
	flag.Parse()
	runtime.GOMAXPROCS(32)
	conf := ReadConfig()
	v(1, "Using config:\n%v", conf)
	dir, err := conf.Directory()
	if err != nil {
		log.Fatalf("unable to set up stenographer directory: %v", err)
	}
	defer dir.Close()

	go runStenotype(conf, dir)
	conf.ExportDebugHandlers(http.DefaultServeMux)
	dir.ExportDebugHandlers(http.DefaultServeMux)
	http.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		queryBytes, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "could not read request body", http.StatusBadRequest)
			return
		}
		queryStr := string(queryBytes)
		log.Printf("Received query %q from %q", queryStr, r.RemoteAddr)
		defer func() {
			log.Printf("Handled query %q from %q in %v", queryStr, r.RemoteAddr, time.Since(start))
		}()
		q, err := query.NewQuery(queryStr)
		if err != nil {
			http.Error(w, "could not parse query", http.StatusBadRequest)
			return
		}
		ctx, cancel := contextFromHTTP(w, r)
		defer cancel()
		packets := dir.Lookup(ctx, q)
		w.Header().Set("Content-Type", "appliation/octet-stream")
		base.PacketsToFile(packets, w)
	})
	log.Fatal(conf.Serve())
}

// contextFromHTTP returns a new context.Content that cancels when the
// underlying http.ResponseWriter closes.
func contextFromHTTP(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if closer, ok := w.(http.CloseNotifier); ok {
		go func() {
			select {
			case <-closer.CloseNotify():
				log.Printf("Detected closed HTTP connection, canceling query")
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}
