//go:build !linux
// +build !linux

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

// Command idiosepius is the minimal 3D Printer server
package main

import "log"

func main() {
	log.SetPrefix("idiosepius: ")
	log.SetFlags(0)
	log.Fatal("idiosepius is supported on linux only")
}
