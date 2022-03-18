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

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func webcam() {
	device := "/dev/video0"
	chosenSize := "640x480"
	listenAddr := ":8080"
	cam, err := openCamera(device)
	if err != nil {
		log.Println("cannot start webcam stream", err)
	}
	defer cam.close()
	formats := cam.formats()
	var format pixelFormat
	for f := range formats {
		if supportedFormats[f] {
			format = f
			break
		}
	}
	if format == 0 {
		log.Fatal("no format available")
	}
	frameSizes := cam.frameSizes(format)
	sort.Slice(frameSizes, func(i, j int) bool {
		return frameSizes[i].widthMax*frameSizes[i].heightMax < frameSizes[j].widthMax*frameSizes[j].heightMax
	})
	var size *frameSize
	for _, f := range frameSizes {
		if chosenSize == f.GetString() {
			size = &f
			break
		}
	}
	if size == nil {
		log.Fatal("No matching frame size, exiting")
	}
	pixelFormat, width, height, err := cam.setImageFormat(format, uint32(size.widthMax), uint32(size.heightMax))
	if err != nil {
		log.Fatal("cannot set image format", err)
	}
	if err := cam.start(); err != nil {
		log.Fatal("cannot start streaming:", err)
	}
	var (
		convertedFrames   = make(chan []byte)
		frameIntake       = make(chan []byte)
		readyForNextFrame = make(chan struct{})
	)
	go encodeToImage(cam, readyForNextFrame, frameIntake, convertedFrames, pixelFormat, width, height)
	go httpServe(listenAddr, convertedFrames)
	for {
		err := cam.waitFrame(5 * time.Second)
		var errFrameTimeout *frameTimeoutError
		if errors.As(err, &errFrameTimeout) {
			log.Println(err)
			continue
		} else if err != nil {
			log.Println(err)
			return
		}
		frame, err := cam.readFrame()
		if err != nil {
			log.Println(err)
			return
		}
		if len(frame) != 0 {
			select {
			case frameIntake <- frame:
				<-readyForNextFrame
			default:
			}
		}
	}
}

const (
	pixelFormatPJPG pixelFormat = 0x47504A50
	pixelFormatYUYV pixelFormat = 0x56595559
)

var supportedFormats = map[pixelFormat]bool{
	pixelFormatPJPG: true,
	pixelFormatYUYV: true,
}

func encodeToImage(wc *stream, readyForNextFrame chan<- struct{}, frameIntake <-chan []byte, convertedFrames chan<- []byte, format pixelFormat, width, height uint32) {
	var (
		frame []byte
		img   image.Image
	)
	for bframe := range frameIntake {
		if len(frame) < len(bframe) {
			frame = make([]byte, len(bframe))
		}
		copy(frame, bframe)
		readyForNextFrame <- struct{}{}
		switch format {
		case pixelFormatYUYV:
			yuyv := image.NewYCbCr(image.Rect(0, 0, int(width), int(height)), image.YCbCrSubsampleRatio422)
			for i := range yuyv.Cb {
				ii := i * 4
				yuyv.Y[i*2] = frame[ii]
				yuyv.Y[i*2+1] = frame[ii+2]
				yuyv.Cb[i] = frame[ii+1]
				yuyv.Cr[i] = frame[ii+3]

			}
			img = yuyv
		default:
			log.Fatal("unknown frame format")
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			log.Fatal("cannot encode frame:", err)
		}
		convertedFrames <- buf.Bytes()
	}
}

func httpServe(addr string, convertedFrames <-chan []byte) {
	var (
		mu          sync.RWMutex
		subscribers = make(map[chan []byte]struct{})
	)
	go func() {
		for convertedFrame := range convertedFrames {
			mu.RLock()
			for subscriber := range subscribers {
				select {
				case subscriber <- convertedFrame:
				default:
				}
			}
			mu.RUnlock()
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println(">", r.RemoteAddr, r.URL)
		defer log.Println("<", r.RemoteAddr, r.URL)
		ch := make(chan []byte)
		mu.Lock()
		subscribers[ch] = struct{}{}
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(subscribers, ch)
			mu.Unlock()
		}()
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		const boundary = `frame`
		w.Header().Set("Content-Type", `multipart/x-mixed-replace;boundary=`+boundary)
		mw := multipart.NewWriter(w)
		mw.SetBoundary(boundary)
		for frame := range convertedFrames {
			jpeg, err := mw.CreatePart(textproto.MIMEHeader{
				"Content-type":   []string{"image/jpeg"},
				"Content-length": []string{strconv.Itoa(len(frame))},
			})
			if err != nil {
				log.Println("cannot create multipart response:", err)
				return
			}
			if _, err := jpeg.Write(frame); err != nil {
				log.Println("cannot write multipart response:", err)
				return
			}
		}
	})
	log.Fatal(http.ListenAndServe(addr, mux))
}

type stream struct {
	fd      uintptr
	buffers [][]byte
}

func openCamera(path string) (*stream, error) {
	handle, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0666)
	if err != nil {
		return nil, fmt.Errorf("cannot open camera at %q: %w", path, err)
	}
	fd := uintptr(handle)
	if fd < 0 {
		return nil, fmt.Errorf("invalid file handle")
	}
	isCaptureDevice, isStreamDevice, err := v4l2CheckCapabilities(fd)
	if err != nil {
		return nil, fmt.Errorf("cannot check camera capabilities: %w", err)
	}
	if !isCaptureDevice {
		return nil, errors.New("not a capture device")
	}
	if !isStreamDevice {
		return nil, errors.New("not a stream device")
	}
	w := &stream{
		fd: uintptr(fd),
	}
	return w, nil
}

// Refer to https://www.kernel.org/doc/html/v4.9/media/uapi/v4l/vidioc-enum-framesizes.html
func (w *stream) formats() map[pixelFormat]string {
	formats := make(map[pixelFormat]string)
	index := uint32(0)
	for {
		code, desc, err := v4l2GetPixelFormat(w.fd, index)
		if err != nil {
			break
		}
		formats[pixelFormat(code)] = desc
		index++
	}
	return formats
}

func (w *stream) frameSizes(f pixelFormat) []frameSize {
	frameSizes := make([]frameSize, 0)
	index := uint32(0)
	for {
		s, err := v4l2GetFrameSize(w.fd, index, uint32(f))
		if err != nil {
			break
		}
		frameSizes = append(frameSizes, s)
		index++
	}
	return frameSizes
}

func (w *stream) setImageFormat(f pixelFormat, width, height uint32) (pixelFormat, uint32, uint32, error) {
	changedCode, changedWidth, changedHeight, err := v4l2SetImageFormat(w.fd, uint32(f), width, height)
	return pixelFormat(changedCode), changedWidth, changedHeight, err
}

func (w *stream) start() error {
	bufferCount := uint32(256)
	bufferCount, err := v4l2MMapRequestBuffers(w.fd, bufferCount)
	if err != nil {
		return fmt.Errorf("cannot memory map request buffers: %w", err)
	}
	w.buffers = make([][]byte, bufferCount, bufferCount)
	for index := range w.buffers {
		buffer, err := v4l2MMapQueryBuffer(w.fd, uint32(index))
		if err != nil {
			return fmt.Errorf("cannot map memory: %w", err)
		}
		w.buffers[index] = buffer
	}
	for i := range w.buffers {
		err := v4l2MMapEnqueueBuffer(w.fd, uint32(i))
		if err != nil {
			return fmt.Errorf("cannot enqueue buffer (%v): %w", i, err)
		}
	}
	if err := v4l2StartStreaming(w.fd); err != nil {
		return fmt.Errorf("cannot start streaming: %w", err)
	}
	return nil
}

func (w *stream) readFrame() ([]byte, error) {
	updatedIndex, updatedLength, err := v4l2MMapDequeueBuffer(w.fd)
	if err != nil {
		return nil, err
	}
	v4l2MMapEnqueueBuffer(w.fd, updatedIndex)
	return w.buffers[int(updatedIndex)][:updatedLength], nil
}

func (w *stream) waitFrame(timeout time.Duration) error {
	count, err := v4l2WaitFrame(w.fd, timeout)
	switch {
	case err != nil:
		return fmt.Errorf("got timeout waiting for frame (%v): %w", timeout, err)
	case count < 0:
		return fmt.Errorf("invalid frame count")
	case count == 0:
		return &frameTimeoutError{}
	default:
		return nil
	}
}

func (w *stream) close() error {
	for _, buffer := range w.buffers {
		err := v4L2MMapReleaseBuffer(buffer)
		if err != nil {
			return err
		}
	}
	v4l2StopStreaming(w.fd)
	return unix.Close(int(w.fd))
}

// Refer to /usr/include/linux/videodev2.h
type pixelFormat uint32

type frameSize struct {
	widthMin     uint32
	widthMax     uint32
	widthStep    uint32
	heightHeight uint32
	heightMax    uint32
	heightStep   uint32
}

func (s frameSize) GetString() string {
	if s.widthStep == 0 && s.heightStep == 0 {
		return fmt.Sprintf("%dx%d", s.widthMax, s.heightMax)
	}
	return fmt.Sprintf("[%d-%d;%d]x[%d-%d;%d]", s.widthMin, s.widthMax, s.widthStep, s.heightHeight, s.heightMax, s.heightStep)
}

type frameTimeoutError struct{}

func (e *frameTimeoutError) Error() string {
	return "frame timeout"
}

var (
	v4l2VidiocQueryCap       = syscallIoctlReadArg(uintptr('V'), 0, unsafe.Sizeof(v4l2Capability{}))
	v4l2VidiocEnumFmt        = syscallIoctlReadWriteArg(uintptr('V'), 2, unsafe.Sizeof(v4l2FormatDescription{}))
	v4l2VidiocSFmt           = syscallIoctlReadWriteArg(uintptr('V'), 5, unsafe.Sizeof(v4l2Format{}))
	v4l2VidiocReqBufs        = syscallIoctlReadWriteArg(uintptr('V'), 8, unsafe.Sizeof(v4l2RequestBuffers{}))
	v4l2VidiocQueryBuf       = syscallIoctlReadWriteArg(uintptr('V'), 9, unsafe.Sizeof(v4l2Buffer{}))
	v4l2VidiocQBuf           = syscallIoctlReadWriteArg(uintptr('V'), 15, unsafe.Sizeof(v4l2Buffer{}))
	v4l2VidiocDQBuf          = syscallIoctlReadWriteArg(uintptr('V'), 17, unsafe.Sizeof(v4l2Buffer{}))
	v4l2VidiocStreamOn       = syscallIoctlWriteArg(uintptr('V'), 18, 4)
	v4l2VidiocStreamOff      = syscallIoctlWriteArg(uintptr('V'), 19, 4)
	v4l2VidiocEnumFrameSizes = syscallIoctlReadWriteArg(uintptr('V'), 74, unsafe.Sizeof(v4l2FrameSizeEnum{}))
	v4l2NativeByteOrder      = v4l2GetNativeByteOrder()
	v4l2PointerHack          = unsafe.Pointer(uintptr(0))
)

const (
	v4l2BufTypeVideoCapture uint32 = 1
	v4l2FieldAny            uint32 = 0

	v4l2CapVideoCapture uint32 = 0x00000001
	v4l2CapStreaming    uint32 = 0x04000000
	v4l2MemoryMMap      uint32 = 1

	v4l2FrameSizeTypeDiscrete   uint32 = 1
	v4l2FrameSizeTypeContinuous uint32 = 2
	v4l2FrameSizeTypeStepwise   uint32 = 3
)

func v4l2GetPixelFormat(fd uintptr, index uint32) (uint32, string, error) {
	desc := &v4l2FormatDescription{}
	desc.index = index
	desc.formatDescriptionType = v4l2BufTypeVideoCapture
	if err := syscallIoctl(fd, v4l2VidiocEnumFmt, uintptr(unsafe.Pointer(desc))); err != nil {
		return 0, "", err
	}
	return desc.pixelFormat, v4l2CStringToGoString(desc.description[:]), nil
}

func v4l2StartStreaming(fd uintptr) (err error) {
	uintPointer := v4l2BufTypeVideoCapture
	return syscallIoctl(fd, v4l2VidiocStreamOn, uintptr(unsafe.Pointer(&uintPointer)))
}

func v4l2StopStreaming(fd uintptr) (err error) {
	uintPointer := v4l2BufTypeVideoCapture
	return syscallIoctl(fd, v4l2VidiocStreamOff, uintptr(unsafe.Pointer(&uintPointer)))
}

func v4l2GetFrameSize(fd uintptr, index uint32, code uint32) (frameSize, error) {
	var frameSize frameSize
	frmsizeenum := &v4l2FrameSizeEnum{}
	frmsizeenum.index = index
	frmsizeenum.pixelFormat = code
	if err := syscallIoctl(fd, v4l2VidiocEnumFrameSizes, uintptr(unsafe.Pointer(frmsizeenum))); err != nil {
		return frameSize, err
	}
	switch frmsizeenum.frameSizeType {
	case v4l2FrameSizeTypeDiscrete:
		discrete := &v4l2FrameSizeDiscrete{}
		if err := binary.Read(bytes.NewBuffer(frmsizeenum.union[:]), v4l2NativeByteOrder, discrete); err != nil {
			return frameSize, err
		}
		frameSize.widthMin = discrete.Width
		frameSize.widthMax = discrete.Width
		frameSize.heightHeight = discrete.Height
		frameSize.heightMax = discrete.Height
	case v4l2FrameSizeTypeContinuous:
	case v4l2FrameSizeTypeStepwise:
		stepwise := &v4l2FrameSizeStepwise{}
		if err := binary.Read(bytes.NewBuffer(frmsizeenum.union[:]), v4l2NativeByteOrder, stepwise); err != nil {
			return frameSize, err
		}
		frameSize.widthMin = stepwise.WidthMin
		frameSize.widthMax = stepwise.WidthMax
		frameSize.widthStep = stepwise.WidthStep
		frameSize.heightHeight = stepwise.HeightMin
		frameSize.heightMax = stepwise.HeightMax
		frameSize.heightStep = stepwise.HeightStep
	}
	return frameSize, nil
}

func v4l2SetImageFormat(fd uintptr, formatCode uint32, width uint32, height uint32) (updatedFormatCode, updatedWidth, updatedHeight uint32, err error) {
	pix := v4l2PixFormat{
		Width:       width,
		Height:      height,
		PixelFormat: formatCode,
		Field:       v4l2FieldAny,
	}
	var pixFormatEncoded bytes.Buffer
	if err := binary.Write(&pixFormatEncoded, v4l2NativeByteOrder, pix); err != nil {
		return formatCode, width, height, fmt.Errorf("cannot emcpde V4L2 Pix Format request: %w", err)
	}
	format := &v4l2Format{
		formatType: v4l2BufTypeVideoCapture,
	}
	copy(format.union.data[:], pixFormatEncoded.Bytes())
	if err := syscallIoctl(fd, v4l2VidiocSFmt, uintptr(unsafe.Pointer(format))); err != nil {
		return formatCode, width, height, fmt.Errorf("cannot IOCTL set V4L2 Pix Format: %w", err)
	}
	decodedPixFormat := &v4l2PixFormat{}
	if err := binary.Read(bytes.NewBuffer(format.union.data[:]), v4l2NativeByteOrder, decodedPixFormat); err != nil {
		return formatCode, width, height, fmt.Errorf("cannot read V4L2 Pix Format: %w", err)
	}
	return decodedPixFormat.PixelFormat, decodedPixFormat.Width, decodedPixFormat.Height, nil
}

func v4l2GetNativeByteOrder() binary.ByteOrder {
	i := int32(0x01020304)
	u := unsafe.Pointer(&i)
	b := *((*byte)(u))
	if b == 0x04 {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

func v4l2WaitFrame(fd uintptr, timeout time.Duration) (count int, err error) {
	for {
		fds := &unix.FdSet{}
		fds.Set(int(fd))
		nativeTimeVal := unix.NsecToTimeval(timeout.Nanoseconds())
		tv := &nativeTimeVal
		count, err = unix.Select(int(fd+1), fds, nil, nil, tv)
		if count < 0 && err == unix.EINTR {
			continue
		}
		return
	}
}

func v4L2MMapReleaseBuffer(buffer []byte) error {
	return unix.Munmap(buffer)
}

func v4l2MMapEnqueueBuffer(fd uintptr, index uint32) (err error) {
	buffer := &v4l2Buffer{
		index:      index,
		bufferType: v4l2BufTypeVideoCapture,
		memory:     v4l2MemoryMMap,
	}
	return syscallIoctl(fd, v4l2VidiocQBuf, uintptr(unsafe.Pointer(buffer)))
}

func v4l2MMapDequeueBuffer(fd uintptr) (updatedIndex, updatedLength uint32, err error) {
	buffer := &v4l2Buffer{
		bufferType: v4l2BufTypeVideoCapture,
		memory:     v4l2MemoryMMap,
	}
	if err := syscallIoctl(fd, v4l2VidiocDQBuf, uintptr(unsafe.Pointer(buffer))); err != nil {
		return 0, 0, err
	}
	return buffer.index, buffer.bytesUsed, nil
}

func v4l2MMapQueryBuffer(fd uintptr, index uint32) (buffer []byte, err error) {
	req := &v4l2Buffer{
		bufferType: v4l2BufTypeVideoCapture,
		memory:     v4l2MemoryMMap,
		index:      index,
	}
	if err = syscallIoctl(fd, v4l2VidiocQueryBuf, uintptr(unsafe.Pointer(req))); err != nil {
		return nil, err
	}
	var offset uint32
	if err := binary.Read(bytes.NewBuffer(req.union[:]), v4l2NativeByteOrder, &offset); err != nil {
		return nil, err
	}
	buffer, err = unix.Mmap(int(fd), int64(offset), int(req.length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	return buffer, err
}

func v4l2CStringToGoString(c []byte) string {
	n := -1
	for i, b := range c {
		if b == 0 {
			break
		}
		n = i
	}
	return string(c[:n+1])
}

func v4l2MMapRequestBuffers(fd uintptr, bufCount uint32) (updatedBufCount uint32, err error) {
	req := &v4l2RequestBuffers{
		count:      bufCount,
		bufferType: v4l2BufTypeVideoCapture,
		memory:     v4l2MemoryMMap,
	}
	if err := syscallIoctl(fd, v4l2VidiocReqBufs, uintptr(unsafe.Pointer(req))); err != nil {
		return bufCount, err
	}
	return req.count, nil
}

func v4l2CheckCapabilities(fd uintptr) (isCapture, isStream bool, err error) {
	caps := &v4l2Capability{}
	if err = syscallIoctl(fd, v4l2VidiocQueryCap, uintptr(unsafe.Pointer(caps))); err != nil {
		return false, false, err
	}
	return (caps.capabilities & v4l2CapVideoCapture) != 0,
		(caps.capabilities & v4l2CapStreaming) != 0,
		nil

}

type v4l2Capability struct {
	driver       [16]uint8
	card         [32]uint8
	busInfo      [32]uint8
	version      uint32
	capabilities uint32
	deviceCaps   uint32
	reserved     [3]uint32
}

type v4l2FormatDescription struct {
	index                 uint32
	formatDescriptionType uint32
	flags                 uint32
	description           [32]uint8
	pixelFormat           uint32
	reserved              [4]uint32
}

type v4l2FrameSizeEnum struct {
	index         uint32
	pixelFormat   uint32
	frameSizeType uint32
	union         [24]uint8
	reserved      [2]uint32
}

type v4l2FrameSizeDiscrete struct {
	Width  uint32
	Height uint32
}

type v4l2FrameSizeStepwise struct {
	WidthMin   uint32
	WidthMax   uint32
	WidthStep  uint32
	HeightMin  uint32
	HeightMax  uint32
	HeightStep uint32
}

// This hack forces the alignment of the v4l2_format inside the ioctl
// VIDIOC_S_FMT calls. Refer to
// https://www.kernel.org/doc/html/v4.9/media/uapi/v4l/vidioc-g-fmt.html#vidioc-g-fmt
type v4l2FormatAlignedUnionHack struct {
	data [200 - unsafe.Sizeof(v4l2PointerHack)]byte
	_    unsafe.Pointer
}

type v4l2Format struct {
	formatType uint32
	union      v4l2FormatAlignedUnionHack
}

type v4l2PixFormat struct {
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	ColorSpace   uint32
	Priv         uint32
	Flags        uint32
	YcbcrEnc     uint32
	Quantization uint32
	XferFunc     uint32
}

type v4l2RequestBuffers struct {
	count      uint32
	bufferType uint32
	memory     uint32
	reserved   [2]uint32
}

type v4l2Buffer struct {
	index      uint32
	bufferType uint32
	bytesUsed  uint32
	flags      uint32
	field      uint32
	timestamp  unix.Timeval
	timeCode   v4l2TimeCode
	sequence   uint32
	memory     uint32
	union      [unsafe.Sizeof(v4l2PointerHack)]uint8
	length     uint32
	reserved2  uint32
	reserved   uint32
}

type v4l2TimeCode struct {
	timeCodeType uint32
	flags        uint32
	frames       uint8
	seconds      uint8
	minutes      uint8
	hours        uint8
	userBits     [4]uint8
}

const (
	typeBits = 8
	typeMask = (1 << typeBits) - 1

	numberBits = 8
	numberMask = (1 << numberBits) - 1

	sizeBits = 14
	sizeMask = (1 << sizeBits) - 1

	directionBits  = 2
	directionMask  = (1 << directionBits) - 1
	directionWrite = 1
	directionRead  = 2

	numberShift    = 0
	typeShift      = numberShift + numberBits
	sizeShift      = numberShift + numberBits + typeBits
	directionShift = numberShift + numberBits + typeBits + sizeBits
)

func syscallIoctlReadArg(typ, number, size uintptr) uintptr {
	return (directionRead << directionShift) | (typ << typeShift) | (number << numberShift) | (size << sizeShift)
}

func syscallIoctlWriteArg(typ, number, size uintptr) uintptr {
	return (directionWrite << directionShift) | (typ << typeShift) | (number << numberShift) | (size << sizeShift)
}

func syscallIoctlReadWriteArg(typ, number, size uintptr) uintptr {
	return ((directionRead | directionWrite) << directionShift) | (typ << typeShift) | (number << numberShift) | (size << sizeShift)
}

func syscallIoctl(fd, op, arg uintptr) error {
	_, _, ep := unix.Syscall(unix.SYS_IOCTL, fd, op, arg)
	if ep != 0 {
		return ep
	}
	return nil
}
