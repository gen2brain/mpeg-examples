package main

import (
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jezek/xgb"
	mitshm "github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xv"
	"github.com/jezek/xgbutil"
	"github.com/jezek/xgbutil/ewmh"
	"github.com/jezek/xgbutil/icccm"

	"github.com/gen2brain/oss"
	"github.com/gen2brain/shm"
	"github.com/jfbus/httprs"

	"github.com/gen2brain/mpeg"
)

var (
	// I420 Intel Indeo 4
	I420 = fourcc("I420")
	// YV12 YUV420P
	YV12 = fourcc("YV12")
)

type app struct {
	mpg *mpeg.MPEG

	width        int
	height       int
	scaledWidth  int
	scaledHeight int

	framerate  float64
	samplerate int

	pause   bool
	running bool
	visible bool
	seekTo  float64

	shmId  int
	shmSeg mitshm.Seg

	data []byte
	dest *image.RGBA

	useXv    bool
	xvPort   xv.Port
	xvFormat xv.Format

	lumaSize   int
	chromaSize int
	frameSize  int

	yi  int
	cbi int
	cri int

	formatID   uint32
	formatName string

	x  *xgb.Conn
	xu *xgbutil.XUtil
	gc xproto.Gcontext

	window xproto.Window
	screen *xproto.ScreenInfo

	device *oss.Audio
}

func newApp(m *mpeg.MPEG) (*app, error) {
	a := &app{}
	a.mpg = m

	a.width = a.mpg.Width()
	a.height = a.mpg.Height()
	a.scaledWidth = a.width
	a.scaledHeight = a.height

	a.framerate = a.mpg.Framerate()
	a.samplerate = a.mpg.Samplerate()

	hasVideo := a.mpg.NumVideoStreams() > 0
	hasAudio := a.mpg.NumAudioStreams() > 0
	a.mpg.SetVideoEnabled(hasVideo)
	a.mpg.SetAudioEnabled(hasAudio)

	var err error
	xgb.Logger = log.New(io.Discard, "", 0)

	a.x, err = xgb.NewConn()
	if err != nil {
		return nil, err
	}

	a.xu, err = xgbutil.NewConnXgb(a.x)
	if err != nil {
		return nil, err
	}

	err = mitshm.Init(a.x)
	if err != nil {
		return nil, err
	}

	a.useXv = true
	err = xv.Init(a.x)
	if err != nil {
		a.useXv = false
	}

	a.screen = xproto.Setup(a.x).DefaultScreen(a.x)
	a.window, err = xproto.NewWindowId(a.x)
	if err != nil {
		return nil, err
	}

	if a.useXv {
		err = a.queryAdaptors()
		if err != nil {
			return nil, err
		}
	}

	xproto.CreateWindow(a.x, a.screen.RootDepth, a.window, a.screen.Root, 0, 0, uint16(a.width), uint16(a.height), 1,
		xproto.WindowClassInputOutput, a.screen.RootVisual,
		xproto.CwBackPixel|xproto.CwEventMask|xproto.CwColormap,
		[]uint32{
			a.screen.BlackPixel,
			xproto.EventMaskKeyPress | xproto.EventMaskExposure | xproto.EventMaskStructureNotify | xproto.EventMaskVisibilityChange,
			uint32(a.screen.DefaultColormap),
		},
	)

	name := []byte(filepath.Base(os.Args[1]))
	xproto.ChangeProperty(a.x, xproto.PropModeReplace, a.window, xproto.AtomWmName, xproto.AtomString, 8, uint32(len(name)), name)

	if !a.useXv {
		hints := icccm.NormalHints{}
		hints.Flags = icccm.SizeHintPMinSize | icccm.SizeHintPMaxSize
		hints.MinWidth = uint(a.width)
		hints.MaxWidth = uint(a.width)
		hints.MinHeight = uint(a.height)
		hints.MaxHeight = uint(a.height)

		err = icccm.WmNormalHintsSet(a.xu, a.window, &hints)
		if err != nil {
			log.Fatal(err)
		}
	}

	xproto.MapWindow(a.x, a.window)

	posX := uint32(int(a.screen.WidthInPixels)/2 - a.width/2)
	posY := uint32(int(a.screen.HeightInPixels)/2 - a.height)
	xproto.ConfigureWindow(a.x, a.window, xproto.ConfigWindowX|xproto.ConfigWindowY, []uint32{posX, posY})

	err = a.createImage()
	if err != nil {
		log.Fatal(err)
	}

	a.gc, err = xproto.NewGcontextId(a.x)
	if err != nil {
		log.Fatal(err)
	}
	xproto.CreateGC(a.x, a.gc, xproto.Drawable(a.window), 0, nil)

	if a.useXv {
		atom := "XV_SYNC_TO_VBLANK"
		reply, err := xproto.InternAtom(a.x, true, uint16(len(atom)), atom).Reply()
		if err == nil {
			xv.SetPortAttribute(a.x, a.xvPort, reply.Atom, 1)
		}
	}

	if hasVideo {
		a.mpg.SetVideoCallback(func(m *mpeg.MPEG, frame *mpeg.Frame) {
			if frame == nil || !a.visible {
				return
			}

			if a.useXv {
				copy(a.data[:a.yi], frame.Y.Data)
				if a.formatID == I420 {
					copy(a.data[a.yi:a.cbi], frame.Cb.Data)
					copy(a.data[a.cbi:a.cri], frame.Cr.Data)
				} else if a.formatID == YV12 {
					copy(a.data[a.cbi:a.cri], frame.Cr.Data)
					copy(a.data[a.yi:a.cbi], frame.Cb.Data)
				}
			} else {
				src := frame.YCbCr()
				for x := 0; x < a.width; x++ {
					for y := 0; y < a.height; y++ {
						yi, ci := src.YOffset(x, y), src.COffset(x, y)
						r, g, b := color.YCbCrToRGB(src.Y[yi], src.Cb[ci], src.Cr[ci])
						i := a.dest.PixOffset(x, y)
						a.dest.Pix[i+0] = b
						a.dest.Pix[i+1] = g
						a.dest.Pix[i+2] = r
						a.dest.Pix[i+3] = 0xff
					}
				}
			}

			a.putImage()
		})
	}

	if hasAudio {
		dev, err := oss.OpenAudio()
		if err != nil {
			log.Fatal(err)
		}
		a.device = dev

		err = dev.SetBufferSize(mpeg.SamplesPerFrame, 2)
		if err != nil {
			log.Fatal(err)
		}

		channels, err := dev.Channels(2)
		if err != nil {
			log.Fatal(err)
		}

		samplerate, err := dev.Samplerate(a.samplerate)
		if err != nil {
			log.Fatal(err)
		}

		format, err := dev.Format(oss.AfmtS16Le)
		if err != nil {
			log.Fatal(err)
		}

		bufferSize, err := dev.BufferSize()
		if err != nil {
			log.Fatal(err)
		}

		log.Printf("OSS: %d channels, %d hz, %v, buffer size %d",
			channels, samplerate, format, bufferSize)

		a.mpg.SetAudioFormat(mpeg.AudioS16)

		duration := float64(mpeg.SamplesPerFrame) / float64(a.samplerate)
		a.mpg.SetAudioLeadTime(time.Duration(duration * float64(time.Second)))

		a.mpg.SetAudioCallback(func(m *mpeg.MPEG, samples *mpeg.Samples) {
			if samples == nil {
				return
			}

			_, err = dev.Write(samples.Bytes())
			if err != nil {
				log.Println(err)
			}
		})
	}

	a.running = true

	go func() {
		for {
			ev, err := a.x.WaitForEvent()
			if err == nil && ev == nil {
				a.running = false
				return
			}

			if err != nil {
				log.Printf("Error: %s\n", err)
			}
			if ev != nil {
				//log.Printf("Event: %v\n", ev)
			}

			a.processEvent(ev)
		}
	}()

	return a, nil
}

func (a *app) processEvent(ev xgb.Event) {
	switch e := ev.(type) {
	case xproto.DestroyNotifyEvent:
		a.running = false
	case xproto.ConfigureNotifyEvent:
		a.scaledWidth = int(e.Width)
		a.scaledHeight = int(e.Height)
	case xproto.ExposeEvent:
		a.putImage()
	case xproto.VisibilityNotifyEvent:
		vne := ev.(xproto.VisibilityNotifyEvent)
		a.visible = vne.State != xproto.VisibilityFullyObscured
	case xproto.KeyPressEvent:
		kpe := ev.(xproto.KeyPressEvent)
		switch {
		case kpe.Detail == 24 || kpe.Detail == 9: // q, escape
			a.running = false
		case kpe.Detail == 65 || kpe.Detail == 33: // space, p
			a.pause = !a.pause
		case kpe.Detail == 41 || kpe.Detail == 95: // f, f11
			_ = ewmh.WmStateReq(a.xu, a.window, ewmh.StateToggle, "_NET_WM_STATE_FULLSCREEN")
		case kpe.Detail == 114: // right
			a.seekTo = a.mpg.Time().Seconds() + 3
		case kpe.Detail == 113: // left
			a.seekTo = a.mpg.Time().Seconds() - 3
		}
	}
}

func (a *app) run() {
	var now, ideal int64
	var lastTime, currentTime, elapsedTime float64

	ideal = time.Now().UnixMilli()

	for a.running {
		if !a.pause {
			currentTime = float64(time.Now().UnixMilli()) / 1000
			elapsedTime = currentTime - lastTime
			if elapsedTime > 1.0/a.framerate {
				elapsedTime = 1.0 / a.framerate
			}
			lastTime = currentTime

			if a.seekTo != -1 {
				a.mpg.Seek(time.Duration(a.seekTo*float64(time.Second)), false)
				a.seekTo = -1
			} else {
				a.mpg.Decode(time.Duration(elapsedTime * float64(time.Second)))
			}
		}

		if a.mpg.HasEnded() {
			return
		}

		ideal += 17
		now = time.Now().UnixMilli()
		if now < ideal {
			time.Sleep(time.Duration(ideal-now) * time.Millisecond)
		}
	}
}

func (a *app) destroy() {
	if a.useXv {
		xv.UngrabPort(a.x, a.xvPort, xproto.TimeCurrentTime)
	}

	_ = a.destroyImage()
	xproto.FreeGC(a.x, a.gc)
	xproto.DestroyWindow(a.x, a.window)
	a.x.Close()

	if a.device != nil {
		a.device.Close()
	}
}

func (a *app) createImage() error {
	var err error

	a.frameSize = a.width * a.height * 4
	if a.useXv {
		mbWidth := (a.width + 15) >> 4
		mbHeight := (a.height + 15) >> 4

		lumaWidth := mbWidth << 4
		lumaHeight := mbHeight << 4
		chromaWidth := mbWidth << 3
		chromaHeight := mbHeight << 3

		a.lumaSize = lumaWidth * lumaHeight
		a.chromaSize = chromaWidth * chromaHeight
		a.frameSize = a.lumaSize + 2*a.chromaSize

		a.yi = a.width * a.height
		a.cbi = a.yi + a.width*a.height/4
		a.cri = a.cbi + a.width*a.height/4
	}

	a.shmId, err = shm.Get(shm.IPC_PRIVATE, a.frameSize, shm.IPC_CREAT|0666)
	if err != nil {
		return err
	}

	a.shmSeg, err = mitshm.NewSegId(a.x)
	if err != nil {
		return err
	}

	a.data, err = shm.At(a.shmId, 0, 0)
	if err != nil {
		return err
	}

	mitshm.Attach(a.x, a.shmSeg, uint32(a.shmId), false)

	if !a.useXv {
		a.dest = &image.RGBA{
			Pix:    a.data,
			Stride: 4 * a.width,
			Rect: image.Rectangle{
				Min: image.Point{},
				Max: image.Point{X: a.width, Y: a.height},
			},
		}
	}

	return nil
}

func (a *app) destroyImage() error {
	err := shm.Dt(a.data)
	if err != nil {
		return err
	}

	err = shm.Rm(a.shmId)
	if err != nil {
		return err
	}

	mitshm.Detach(a.x, a.shmSeg)

	return nil
}

func (a *app) putImage() {
	if a.useXv {
		err := xv.ShmPutImageChecked(a.x, a.xvPort, xproto.Drawable(a.window), a.gc, a.shmSeg, a.formatID, 0, 0, 0,
			uint16(a.width), uint16(a.height), 0, 0, uint16(a.scaledWidth), uint16(a.scaledHeight), uint16(a.width), uint16(a.height), 0).Check()
		if err != nil {
			log.Println(err)
		}
	} else {
		err := mitshm.PutImageChecked(a.x, xproto.Drawable(a.window), a.gc, uint16(a.width), uint16(a.height),
			0, 0, uint16(a.width), uint16(a.height), 0, 0, a.screen.RootDepth, xproto.ImageFormatZPixmap, 0, a.shmSeg, 0).Check()
		if err != nil {
			log.Println(err)
		}
	}

	a.x.Sync()
}

func (a *app) queryAdaptors() error {
	var xvName string
	var gotPort, haveI420, haveYV12 bool

	reply, err := xv.QueryAdaptors(a.x, a.screen.Root).Reply()
	if err != nil {
		return err
	}

	for _, info := range reply.Info {
		xvName = info.Name

		if info.Type&xv.TypeImageMask == 0 || info.Type&xv.TypeInputMask == 0 {
			continue
		}

		r, err := xv.ListImageFormats(a.x, info.BaseId).Reply()
		if err != nil {
			log.Fatal(err)
		}

		for _, format := range r.Format {
			if format.Id == I420 {
				haveI420 = true
			} else if format.Id == YV12 {
				haveYV12 = true
			}
		}

		if !haveI420 && !haveYV12 {
			continue
		}

		if haveI420 {
			a.formatID = I420
			a.formatName = "I420"
		} else if haveYV12 {
			a.formatID = YV12
			a.formatName = "YV12"
		}

		for p := 0; p < int(info.NumPorts); p++ {
			a.xvPort = info.BaseId + xv.Port(p)
			r, err := xv.GrabPort(a.x, a.xvPort, xproto.TimeCurrentTime).Reply()
			if err == nil && r.Result == 0 {
				gotPort = true
				break
			}
		}

		for _, format := range info.Formats {
			if format.Depth != a.screen.RootDepth {
				continue
			}

			a.xvFormat = format
			break
		}

		if gotPort {
			break
		}
	}

	if reply.NumAdaptors == 0 {
		log.Println("Error: no Xv adaptor found")
		a.useXv = false
	} else if !haveI420 && !haveYV12 {
		log.Println("Error: no supported format found")
		a.useXv = false
	} else if !gotPort {
		log.Println("Error: could not grab any port")
		a.useXv = false
	}

	if !a.useXv {
		log.Println("Warning: using software fallback")
	} else {
		log.Printf("XVideo: %s, port %d, id: 0x%x (%s)", xvName, a.xvPort, a.formatID, a.formatName)
	}

	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println(fmt.Sprintf("Usage: %s <file or url>", filepath.Base(os.Args[0])))
		os.Exit(1)
	}

	r, err := openFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()

	mpg, err := mpeg.New(r)
	if err != nil {
		log.Fatal(err)
	}

	app, err := newApp(mpg)
	if err != nil {
		log.Fatal(err)
	}
	defer app.destroy()

	app.run()
}

func openFile(arg string) (io.ReadSeekCloser, error) {
	var err error
	var r io.ReadSeekCloser

	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		res, err := http.Get(arg)
		if err != nil {
			return nil, err
		}

		r = httprs.NewHttpReadSeeker(res)
	} else {
		r, err = os.Open(arg)
		if err != nil {
			return nil, err
		}
	}

	return r, nil
}

func fourcc(b string) uint32 {
	return uint32(b[0]) | (uint32(b[1]) << 8) | (uint32(b[2]) << 16) | (uint32(b[3]) << 24)
}
