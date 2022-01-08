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

type Configuration struct {
	Printer Printer `ini:"printer"`
	Wifi    Wifi    `ini:"wifi"`
}

type Printer struct {
	Port string `ini:"port"`
	Rate int    `ini:"rate"`
}

type Wifi struct {
	SSID    string `ini:"ssid"`
	PSK     string `ini:"psk"`
	Country string `ini:"country"`
	IP      string `ini:"ip"`
	Gateway string `ini:"gateway"`
	DNS     string `ini:"dns"`
}
