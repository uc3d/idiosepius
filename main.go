/*
Copyright 2022 github.com/uc3d (U. Cirello)

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

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"

	"cirello.io/oversight"
	"gopkg.in/ini.v1"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.SetPrefix("muprint: ")
	log.SetFlags(0)

	var config Configuration
	if err := ini.MapTo(&config, "muprint.ini"); err != nil {
		log.Fatal("cannot open configuration file:", err)
	}

	tree := oversight.New(
		oversight.WithRestartStrategy(oversight.OneForOne()),
		oversight.WithLogger(log.Default()),
	)

	// Start web server
	webServer := &http.Server{Addr: "0.0.0.0:80", Handler: webserverMux()}
	go func() {
		<-ctx.Done()
		webServer.Shutdown(context.TODO())
	}()
	tree.Add(func(ctx context.Context) error {
		if err := webServer.ListenAndServe(); err != nil {
			return fmt.Errorf("cannot keep serving anymore: %w", err)
		}
		return nil
	})
	// TODO: connect to printer
	// TODO: start OctoPrint server

	if err := tree.Start(ctx); err != nil {
		log.Fatal("tree errored:", err)
	}
}
