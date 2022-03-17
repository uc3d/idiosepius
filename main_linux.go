//go:build linux
// +build linux

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

// Command idiosepius is the minimal 3D Printer server.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	log.SetPrefix("")
	log.SetFlags(0)
	switch filepath.Base(os.Args[0]) {
	case "terminal":
		terminal()
	default:
		print()
	}
}

func terminal() {
	if len(os.Args) != 3 {
		log.Println("usage: terminal /dev/serial BAUDRATE")
		return
	}
	portfn, err := filepath.Abs(os.Args[1])
	if err != nil {
		log.Println("cannot find serial port absolute path:", err)
		os.Exit(255)
	}
	baudrate := os.Args[2]
	log.Println("opening serial port", portfn)
	port, err := openSerial(portfn, baudrate)
	if err != nil {
		log.Println("cannot open serial device:", err)
		os.Exit(255)
	}
	defer port.Close()
	conn := io.MultiWriter(
		&prefixedWriter{"send: ", os.Stderr},
		port,
	)
	log.Println("press ^D to exit")
	gcodereader := gcodeParser{r: os.Stdin}
	for gcodereader.Scan() {
		fmt.Fprintln(conn, gcodereader.Text())
		parseGCodeResponse(context.Background(), port)
	}
}

func print() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(os.Args) != 4 {
		log.Println("usage: idiosepius print.gcode /dev/serial BAUDRATE")
		return
	}
	gcodefn, err := filepath.Abs(os.Args[1])
	if err != nil {
		log.Println("cannot find absolute path for gcode file:", err)
		os.Exit(255)
	}
	log.Println("opening", gcodefn)
	gcodefd, err := os.Open(gcodefn)
	if err != nil {
		log.Println("cannot open gcode file:", err)
		os.Exit(255)
	}
	defer gcodefd.Close()
	portfn, err := filepath.Abs(os.Args[2])
	if err != nil {
		log.Println("cannot find serial port absolute path:", err)
		os.Exit(255)
	}
	baudrate := os.Args[3]
	log.Println("opening serial port", portfn)
	port, err := openSerial(portfn, baudrate)
	if err != nil {
		log.Println("cannot open serial device:", err)
		os.Exit(255)
	}
	defer port.Close()
	handshake(ctx, port)
	commands := &gcodeCommandCount{serial: io.MultiWriter(
		&prefixedWriter{"send: ", os.Stderr},
		port,
	)}
	const (
		// resets the firmware's line count and start the printing timer
		printPreamble = "M110 N0\nM75\n"
		// stops the printing timer
		printEpilogue = "M77\n"
	)
	gcodereader := gcodeParser{
		r: io.MultiReader(
			strings.NewReader(printPreamble),
			gcodefd,
			strings.NewReader(printEpilogue),
		),
	}
	log.Println("starting printing")
	for gcodereader.Scan() {
		if ctx.Err() != nil {
			log.Println("interrupting...")
			break
		}
		movement := gcodereader.Text()
		commands.send(movement)
		parseGCodeResponse(ctx, port)
	}
	if err := gcodereader.Err(); err != nil {
		log.Println("some error happened during gcode parsing:", err)
		os.Exit(253)
	}
	log.Println("printing complete")
}

type gcodeParser struct {
	r       io.Reader
	scanner *bufio.Scanner
	line    string
}

func (gp *gcodeParser) init() {
	if gp.scanner == nil {
		gp.scanner = bufio.NewScanner(gp.r)
	}
}

func (gp *gcodeParser) Err() error {
	gp.init()
	return gp.scanner.Err()
}

func (gp *gcodeParser) Scan() bool {
	gp.init()
	for gp.scanner.Scan() {
		line, _, _ := strings.Cut(gp.scanner.Text(), ";")
		if strings.TrimSpace(line) == "" {
			continue
		}
		gp.line = line
		return true
	}
	return false
}

func (gp *gcodeParser) Text() string {
	gp.init()
	return gp.line
}

type gcodeCommandCount struct {
	serial    io.Writer
	lineCount int
}

func (gc *gcodeCommandCount) send(command string) {
	explanation, ok := gcodeExplanations[extractGCodeCommand(command)]
	if ok {
		explanation = "; " + explanation
	}
	command = fmt.Sprintf("N%d %s", gc.lineCount, command)
	fmt.Fprintf(gc.serial, "%s*%d%s\n", command, checksum(command), explanation)
	gc.lineCount++
}

func checksum(cmd string) int {
	var cs byte
	for i := 0; i < len(cmd); i++ {
		cs = cs ^ cmd[i]
	}
	cs &= 0xff
	return int(cs)
}

func openSerial(name string, baudrate string) (p *serialPort, err error) {
	f, err := os.OpenFile(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0666)
	if err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic recovery: %v", r)
		}
		if err != nil && f != nil {
			f.Close()
		}
	}()
	// typical octoprint setup:
	// baudrate=115200, bytesize=8, parity='N', stopbits=1, timeout=None
	// Base settings
	cFlag := uint32(unix.CREAD | unix.CLOCAL)
	cFlag |= unix.CS8 // bytesize = 8
	// parity = NONE -- is the default, do nothing
	// 1 stop bit -- is the default, do nothing
	t := unix.Termios{
		Iflag: unix.IGNPAR,
		Cflag: cFlag,
	}
	t.Cc[unix.VMIN] = 1  // block until it sees one byte
	t.Cc[unix.VTIME] = 0 // never times out
	fd := f.Fd()
	setTerminal := func(t unix.Termios) error {
		if _, _, errno := unix.Syscall6(
			unix.SYS_IOCTL,
			uintptr(fd), uintptr(unix.TCSETS), uintptr(unsafe.Pointer(&t)),
			0, 0, 0,
		); errno != 0 {
			return errno
		}
		return nil
	}
	switch baudrate {
	case "19200":
		t.Ispeed = unix.B19200
		t.Ospeed = unix.B19200
		cFlag |= unix.B19200
		if err := setTerminal(t); err != nil {
			return nil, err
		}
	case "38400":
		t.Ispeed = unix.B38400
		t.Ospeed = unix.B38400
		cFlag |= unix.B38400
		if err := setTerminal(t); err != nil {
			return nil, err
		}
	case "57600":
		t.Ispeed = unix.B57600
		t.Ospeed = unix.B57600
		cFlag |= unix.B57600
		if err := setTerminal(t); err != nil {
			return nil, err
		}
	case "115200":
		t.Ispeed = unix.B115200
		t.Ospeed = unix.B115200
		cFlag |= unix.B115200
		if err := setTerminal(t); err != nil {
			return nil, err
		}
	case "250000":
		if err := setTerminal(t); err != nil {
			return nil, err
		}
		settings, err := unix.IoctlGetTermios(int(fd), unix.TCGETS2)
		if err != nil {
			return nil, err
		}
		settings.Cflag &^= unix.CBAUD
		settings.Cflag |= unix.BOTHER
		settings.Ispeed = 250000
		settings.Ospeed = 250000
		if err := unix.IoctlSetTermios(int(fd), unix.TCSETS2, settings); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid baud rate: %q", baudrate)
	}
	if err = unix.SetNonblock(int(fd), false); err != nil {
		return
	}
	return &serialPort{f: f}, nil
}

type serialPort struct {
	f *os.File
}

func (p *serialPort) Read(b []byte) (n int, err error) {
	return p.f.Read(b)
}

func (p *serialPort) Write(b []byte) (n int, err error) {
	return p.f.Write(b)
}

func (p *serialPort) Flush() error {
	const TCFLSH = 0x540B
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(p.f.Fd()),
		uintptr(TCFLSH),
		uintptr(unix.TCIOFLUSH),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func (p *serialPort) Close() (err error) {
	return p.f.Close()
}

func handshake(ctx context.Context, port *serialPort) {
	fmt.Fprintln(port, "M110 N0")
	done := make(chan struct{})
	go func() {
		parseGCodeResponse(ctx, port)
		port.Flush()
		close(done)
		log.Println("handshake complete")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Fatal("handshake timeout")
	}
}

func parseGCodeResponse(ctx context.Context, port io.Reader) {
	downstream := bufio.NewScanner(port)
	for downstream.Scan() {
		if ctx.Err() != nil {
			return
		}
		l := downstream.Text()
		log.Println("recv:", l)
		if strings.HasPrefix(l, "ok") {
			break
		} else if l == "//action:" {
			_, req, _ := strings.Cut(l, "//action:")
			log.Println("printer asked to ", req)
			switch req {
			case "poweroff":
				os.Exit(128)
			case "cancel":
				os.Exit(0)
			default:
				log.Println("does not know how to", req)
			}
		}
	}
	if err := downstream.Err(); err != nil {
		log.Println("something bad happened in the server:", err)
		os.Exit(253)
	}
}

type prefixedWriter struct {
	prefix string
	w      io.Writer
}

func (w *prefixedWriter) Write(p []byte) (int, error) {
	if _, err := w.w.Write([]byte(w.prefix)); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

var gcodeExplanations = map[string]string{
	"G0":    "Linear Move",
	"G1":    "Linear Move",
	"G2":    "Arc or Circle Move",
	"G3":    "Arc or Circle Move",
	"G4":    "Dwell",
	"G5":    "Bézier cubic spline",
	"G6":    "Direct Stepper Move",
	"G10":   "Retract",
	"G11":   "Recover",
	"G12":   "Clean the Nozzle",
	"G17":   "CNC Workspace Planes",
	"G18":   "CNC Workspace Planes",
	"G19":   "CNC Workspace Planes",
	"G20":   "Inch Units",
	"G21":   "Millimeter Units",
	"G26":   "Mesh Validation Pattern",
	"G27":   "Park toolhead",
	"G28":   "Auto Home",
	"G29":   "Bed Leveling",
	"G30":   "Single Z-Probe",
	"G31":   "Dock Sled",
	"G32":   "Undock Sled",
	"G33":   "Delta Auto Calibration",
	"G34":   "Z Steppers Auto-Alignment / Mechanical Gantry Calibration",
	"G35":   "Tramming Assistant",
	"G38.2": "Probe target",
	"G38.3": "Probe target",
	"G38.4": "Probe target",
	"G38.5": "Probe target",
	"G42":   "Move to mesh coordinate",
	"G53":   "Move in Machine Coordinates",
	"G60":   "Save Current Position",
	"G61":   "Return to Saved Position",
	"G76":   "Probe temperature calibration",
	"G80":   "Cancel Current Motion Mode",
	"G90":   "Absolute Positioning",
	"G91":   "Relative Positioning",
	"G92":   "Set Position",
	"G425":  "Backlash Calibration",
	"M0":    "Unconditional stop",
	"M1":    "Unconditional stop",
	"M3":    "Spindle CW / Laser On",
	"M4":    "Spindle CCW / Laser On",
	"M5":    "Spindle / Laser Off",
	"M7":    "Coolant Controls",
	"M8":    "Coolant Controls",
	"M9":    "Coolant Controls",
	"M10":   "Vacuum / Blower Control",
	"M11":   "Vacuum / Blower Control",
	"M16":   "Expected Printer Check",
	"M17":   "Enable Steppers",
	"M18":   "Disable steppers",
	"M20":   "List SD Card",
	"M21":   "Init SD card",
	"M22":   "Release SD card",
	"M23":   "Select SD file",
	"M24":   "Start or Resume SD print",
	"M25":   "Pause SD print",
	"M26":   "Set SD position",
	"M27":   "Report SD print status",
	"M28":   "Start SD write",
	"M29":   "Stop SD write",
	"M30":   "Delete SD file",
	"M31":   "Print time",
	"M32":   "Select and Start",
	"M33":   "Get Long Path",
	"M34":   "SDCard Sorting",
	"M42":   "Set Pin State",
	"M43":   "Toggle Debug Pins",
	"M48":   "Probe Repeatability Test",
	"M73":   "Set Print Progress",
	"M75":   "Start Print Job Timer",
	"M76":   "Pause Print Job",
	"M77":   "Stop Print Job Timer",
	"M78":   "Print Job Stats",
	"M80":   "Power On",
	"M81":   "Power Off",
	"M82":   "E Absolute",
	"M83":   "E Relative",
	"M84":   "Disable steppers",
	"M85":   "Inactivity Shutdown",
	"M92":   "Set Axis Steps-per-unit",
	"M100":  "Free Memory",
	"M104":  "Set Hotend Temperature",
	"M105":  "Report Temperatures",
	"M106":  "Set Fan Speed",
	"M107":  "Fan Off",
	"M108":  "Break and Continue",
	"M109":  "Wait for Hotend Temperature",
	"M110":  "Set Line Number",
	"M111":  "Debug Level",
	"M112":  "Emergency Stop",
	"M113":  "Host Keepalive",
	"M114":  "Get Current Position",
	"M115":  "Firmware Info",
	"M117":  "Set LCD Message",
	"M118":  "Serial print",
	"M119":  "Endstop States",
	"M120":  "Enable Endstops",
	"M121":  "Disable Endstops",
	"M122":  "TMC Debugging",
	"M123":  "Fan Tachometers",
	"M125":  "Park Head",
	"M126":  "Baricuda 1 Open",
	"M127":  "Baricuda 1 Close",
	"M128":  "Baricuda 2 Open",
	"M129":  "Baricuda 2 Close",
	"M140":  "Set Bed Temperature",
	"M141":  "Set Chamber Temperature",
	"M143":  "Set Laser Cooler Temperature",
	"M145":  "Set Material Preset",
	"M149":  "Set Temperature Units",
	"M150":  "Set RGB(W) Color",
	"M154":  "Position Auto-Report",
	"M155":  "Temperature Auto-Report",
	"M163":  "Set Mix Factor",
	"M164":  "Save Mix",
	"M165":  "Set Mix",
	"M166":  "Gradient Mix",
	"M190":  "Wait for Bed Temperature",
	"M191":  "Wait for Chamber Temperature",
	"M192":  "Wait for Probe temperature",
	"M193":  "Set Laser Cooler Temperature",
	"M200":  "Set Filament Diameter",
	"M201":  "Set Print Max Acceleration",
	"M203":  "Set Max Feedrate",
	"M204":  "Set Starting Acceleration",
	"M205":  "Set Advanced Settings",
	"M206":  "Set Home Offsets",
	"M207":  "Set Firmware Retraction",
	"M208":  "Firmware Recover",
	"M209":  "Set Auto Retract",
	"M211":  "Software Endstops",
	"M217":  "Filament swap parameters",
	"M218":  "Set Hotend Offset",
	"M220":  "Set Feedrate Percentage",
	"M221":  "Set Flow Percentage",
	"M226":  "Wait for Pin State",
	"M240":  "Trigger Camera",
	"M250":  "LCD Contrast",
	"M256":  "LCD Brightness",
	"M260":  "I2C Send",
	"M261":  "I2C Request",
	"M280":  "Servo Position",
	"M281":  "Edit Servo Angles",
	"M282":  "Detach Servo",
	"M290":  "Babystep",
	"M300":  "Play Tone",
	"M301":  "Set Hotend PID",
	"M302":  "Cold Extrude",
	"M303":  "PID autotune",
	"M304":  "Set Bed PID",
	"M305":  "User Thermistor Parameters",
	"M350":  "Set micro-stepping",
	"M351":  "Set Microstep Pins",
	"M355":  "Case Light Control",
	"M360":  "SCARA Theta A",
	"M361":  "SCARA Theta-B",
	"M362":  "SCARA Psi-A",
	"M363":  "SCARA Psi-B",
	"M364":  "SCARA Psi-C",
	"M380":  "Activate Solenoid",
	"M381":  "Deactivate Solenoids",
	"M400":  "Finish Moves",
	"M401":  "Deploy Probe",
	"M402":  "Stow Probe",
	"M403":  "MMU2 Filament Type",
	"M404":  "Set Filament Diameter",
	"M405":  "Filament Width Sensor On",
	"M406":  "Filament Width Sensor Off",
	"M407":  "Filament Width",
	"M410":  "Quickstop",
	"M412":  "Filament Runout",
	"M413":  "Power-loss Recovery",
	"M420":  "Bed Leveling State",
	"M421":  "Set Mesh Value",
	"M422":  "Set Z Motor XY",
	"M423":  "X Twist Compensation",
	"M425":  "Backlash compensation",
	"M428":  "Home Offsets Here",
	"M430":  "Power Monitor",
	"M486":  "Cancel Objects",
	"M500":  "Save Settings",
	"M501":  "Restore Settings",
	"M502":  "Factory Reset",
	"M503":  "Report Settings",
	"M504":  "Validate EEPROM contents",
	"M510":  "Lock Machine",
	"M511":  "Unlock Machine",
	"M512":  "Set Passcode",
	"M524":  "Abort SD print",
	"M540":  "Endstops Abort SD",
	"M569":  "Set TMC stepping mode",
	"M575":  "Serial baud rate",
	"M600":  "Filament Change",
	"M603":  "Configure Filament Change",
	"M605":  "Multi Nozzle Mode",
	"M665":  "Delta / SCARA Configuration",
	"M666":  "Set Delta / dual endstop adjustments",
	"M672":  "Duet Smart Effector sensitivity",
	"M701":  "Load filament",
	"M702":  "Unload filament",
	"M710":  "Controller Fan settings",
	"M808":  "Repeat Marker",
	"M810":  "G-code macros",
	"M811":  "G-code macros",
	"M812":  "G-code macros",
	"M813":  "G-code macros",
	"M814":  "G-code macros",
	"M815":  "G-code macros",
	"M816":  "G-code macros",
	"M817":  "G-code macros",
	"M818":  "G-code macros",
	"M819":  "G-code macros",
	"M851":  "XYZ Probe Offset",
	"M852":  "Bed Skew Compensation",
	"M860":  "I2C Position Encoders",
	"M861":  "I2C Position Encoders",
	"M862":  "I2C Position Encoders",
	"M863":  "I2C Position Encoders",
	"M864":  "I2C Position Encoders",
	"M865":  "I2C Position Encoders",
	"M866":  "I2C Position Encoders",
	"M867":  "I2C Position Encoders",
	"M868":  "I2C Position Encoders",
	"M869":  "I2C Position Encoders",
	"M871":  "Probe temperature config",
	"M876":  "Handle Prompt Response",
	"M900":  "Linear Advance Factor",
	"M906":  "Stepper Motor Current",
	"M907":  "Set Motor Current",
	"M908":  "Set Trimpot Pins",
	"M909":  "DAC Print Values",
	"M910":  "Commit DAC to EEPROM",
	"M911":  "TMC OT Pre-Warn Condition",
	"M912":  "Clear TMC OT Pre-Warn",
	"M913":  "Set Hybrid Threshold Speed",
	"M914":  "TMC Bump Sensitivity",
	"M915":  "TMC Z axis calibration",
	"M916":  "L6474 Thermal Warning Test",
	"M917":  "L6474 Overcurrent Warning Test",
	"M918":  "L6474 Speed Warning Test",
	"M919":  "TMC Chopper Timing",
	"M928":  "Start SD Logging",
	"M951":  "Magnetic Parking Extruder",
	"M993":  "SD / SPI Flash",
	"M994":  "SD / SPI Flash",
	"M995":  "Touch Screen Calibration",
	"M997":  "Firmware update",
	"M999":  "STOP Restart",
	"M7219": "MAX7219 Control",
	"T0":    "Tool Changed (0)",
	"T1":    "Tool Changed (1)",
	"T2":    "Tool Changed (2)",
	"T3":    "Tool Changed (3)",
	"T4":    "Tool Changed (4)",
	"T5":    "Tool Changed (5)",
	"T6":    "Tool Changed (6)",
}

func extractGCodeCommand(movement string) string {
	m, _, _ := strings.Cut(movement, ";")
	s := bufio.NewScanner(strings.NewReader(m))
	s.Split(scanGCodes)
	for s.Scan() {
		block := s.Text()
		switch block[0] {
		case 'G', 'M', 'T', 'g', 'm', 't':
			return strings.ToUpper(block)
		default:
			continue
		}
	}
	// ignore s.Err() - in the worst case, it doesn't print the explanation.
	return ""
}

func scanGCodes(data []byte, atEOF bool) (advance int, token []byte, err error) {
	start := 0
	for width, i := 0, start; i < len(data); i += width {
		var r rune
		r, width = utf8.DecodeRune(data[i:])
		if unicode.IsSpace(r) {
			return i + width, data[start:i], nil
		}
		nextR, nextWidth := utf8.DecodeRune(data[i+1:])
		if nextR >= 'A' && nextR <= 'Z' || nextR >= 'a' && nextR <= 'z' || unicode.IsSpace(nextR) {
			return i + 1 + nextWidth, data[start : i+1], nil
		}
	}
	if atEOF && len(data) > start {
		return len(data), data[start:], nil
	}
	return start, nil, nil
}
