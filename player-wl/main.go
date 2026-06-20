package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"codeberg.org/tesselslate/wl"
	"codeberg.org/tesselslate/wl-protocols/wp"
	"codeberg.org/tesselslate/wl-protocols/xdg"
	"codeberg.org/tesselslate/wl-protocols/zxdg"
	"golang.org/x/sys/unix"

	"github.com/gen2brain/alsa"
	"github.com/jfbus/httprs"

	"github.com/gen2brain/mpeg"
)

// shmBuf is a wl_buffer plus its mmap slice; it cycles free/in-flight via release.
type shmBuf struct {
	wl   wl.Buffer
	data []byte
}

// player holds state for the single Wayland/decode goroutine (tesselslate
// dispatches synchronously); only ALSA playback runs on its own goroutine.
type player struct {
	mpg           *mpeg.MPEG
	width, height int
	framerate     float64

	dpy          *wl.Display
	surface      wl.Surface
	presentation wp.Presentation
	hasPresent   bool
	useXBGR      bool

	viewport      wp.Viewport
	hasViewport   bool
	winW, winH    int
	viewportDirty bool

	toplevel   xdg.Toplevel
	output     wl.Output
	hasOutput  bool
	audioCh    chan []int16
	paused     bool
	fullscreen bool

	free    []*shmBuf
	present *shmBuf // set by the video callback for the current frame

	last    time.Time
	started bool
	quit    bool

	presented  int
	discarded  int
	lastNs     uint64
	accumNs    uint64
	accumCount int
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println(fmt.Sprintf("Usage: %s <file or url>", filepath.Base(os.Args[0])))
		os.Exit(1)
	}

	r, err := openFile(os.Args[1])
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	defer r.Close()

	mpg, err := mpeg.New(r)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	if mpg.NumVideoStreams() == 0 {
		fmt.Println("no video stream")
		os.Exit(1)
	}

	hasAudio := mpg.NumAudioStreams() > 0
	mpg.SetVideoEnabled(true)
	mpg.SetAudioEnabled(hasAudio)

	p := &player{
		mpg:       mpg,
		width:     mpg.Width(),
		height:    mpg.Height(),
		framerate: mpg.Framerate(),
	}
	samplerate := mpg.Samplerate()

	p.dpy, err = wl.NewDisplay("")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer p.dpy.Close()

	var compositor wl.Compositor
	var shm wl.Shm
	var xdgWmBase xdg.WmBase
	var viewporter wp.Viewporter
	var decoManager zxdg.DecorationManagerV1
	var haveCompositor, haveShm, haveWmBase, haveViewporter, haveDeco bool
	shmFormats := make(map[wl.ShmFormat]bool)

	registry := p.dpy.GetRegistry()
	registry.SetListener(wl.RegistryListener{
		Global: func(_ any, self wl.Registry, name uint32, iface string, version uint32) error {
			switch iface {
			case "wl_compositor":
				compositor = wl.Compositor(self.Bind(name, &wl.CompositorInterface, version))
				haveCompositor = true
			case "wl_shm":
				shm = wl.Shm(self.Bind(name, &wl.ShmInterface, version))
				haveShm = true
				shm.SetListener(wl.ShmListener{
					Format: func(_ any, _ wl.Shm, format wl.ShmFormat) error {
						shmFormats[format] = true
						return nil
					},
				}, nil)
			case "xdg_wm_base":
				xdgWmBase = xdg.WmBase(self.Bind(name, &xdg.WmBaseInterface, version))
				haveWmBase = true
			case "wp_presentation":
				p.presentation = wp.Presentation(self.Bind(name, &wp.PresentationInterface, version))
				p.hasPresent = true
			case "wp_viewporter":
				viewporter = wp.Viewporter(self.Bind(name, &wp.ViewporterInterface, version))
				haveViewporter = true
			case "zxdg_decoration_manager_v1":
				decoManager = zxdg.DecorationManagerV1(self.Bind(name, &zxdg.DecorationManagerV1Interface, version))
				haveDeco = true
			case "wl_output":
				// Fullscreen target; a null output crashes on wl-protocols v0.3.47.
				if !p.hasOutput {
					p.output = wl.Output(self.Bind(name, &wl.OutputInterface, version))
					p.hasOutput = true
				}
			case "wl_seat":
				seat := wl.Seat(self.Bind(name, &wl.SeatInterface, version))
				gotKbd := false
				seat.SetListener(wl.SeatListener{
					Capabilities: func(_ any, self wl.Seat, caps wl.SeatCapability) error {
						if !gotKbd && caps&wl.SeatCapabilityKeyboard != 0 {
							gotKbd = true
							kbd := self.GetKeyboard()
							kbd.SetListener(wl.KeyboardListener{
								Key: func(_ any, _ wl.Keyboard, _, _, key uint32, state wl.KeyboardKeyState) error {
									if state == wl.KeyboardKeyStatePressed {
										p.onKey(key)
									}
									return nil
								},
							}, nil)
						}
						return nil
					},
				}, nil)
			}
			return nil
		},
	}, nil)

	// First roundtrip delivers the globals; the second flushes the shm formats.
	must(p.dpy.Roundtrip())
	must(p.dpy.Roundtrip())

	if !haveCompositor || !haveShm || !haveWmBase {
		fmt.Println("missing required globals (wl_compositor, wl_shm, xdg_wm_base)")
		os.Exit(1)
	}
	if !p.hasPresent {
		fmt.Println("wp_presentation not available; no presentation feedback")
	}

	p.useXBGR = shmFormats[wl.ShmFormatXbgr8888]
	format := wl.ShmFormatXrgb8888
	if p.useXBGR {
		format = wl.ShmFormatXbgr8888
	}

	xdgWmBase.SetListener(xdg.WmBaseListener{
		Ping: func(_ any, self xdg.WmBase, serial uint32) error {
			self.Pong(serial)
			return nil
		},
	}, nil)

	p.surface = compositor.CreateSurface()

	// A viewport lets the compositor scale the native buffer to the window size.
	if haveViewporter {
		p.viewport = viewporter.GetViewport(p.surface)
		p.hasViewport = true
		p.winW, p.winH = p.width, p.height
		p.viewportDirty = true
	}

	xdgSurface := xdgWmBase.GetXdgSurface(p.surface)
	xdgSurface.SetListener(xdg.SurfaceListener{
		Configure: func(_ any, self xdg.Surface, serial uint32) error {
			self.AckConfigure(serial)
			if !p.started {
				p.started = true
				p.renderFrame()
			}
			return nil
		},
	}, nil)

	toplevel := xdgSurface.GetToplevel()
	toplevel.SetListener(xdg.ToplevelListener{
		Configure: func(_ any, _ xdg.Toplevel, width int32, height int32, states []byte) error {
			if p.hasViewport && width > 0 && height > 0 {
				p.winW, p.winH = int(width), int(height)
				p.viewportDirty = true
			}
			return nil
		},
		Close: func(_ any, _ xdg.Toplevel) error {
			p.quit = true
			return nil
		},
	}, nil)
	toplevel.SetTitle(filepath.Base(os.Args[1]))
	toplevel.SetAppId("player-wl")
	p.toplevel = toplevel

	// Ask the compositor to draw the window decorations (title bar, borders).
	if haveDeco {
		deco := decoManager.GetToplevelDecoration(toplevel)
		deco.SetMode(zxdg.ToplevelDecorationV1ModeServerSide)
	}

	// Buffer pool: one mmap holding several frames, recycled on release.
	const nbuf = 4
	stride := p.width * 4
	frameSize := stride * p.height
	totalSize := frameSize * nbuf

	fd, err := unix.MemfdCreate("player-wl", 0)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer syscall.Close(fd)
	must(unix.Ftruncate(fd, int64(totalSize)))
	data, err := syscall.Mmap(fd, 0, totalSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer syscall.Munmap(data)

	pool := shm.CreatePool(fd, int32(totalSize))
	for i := 0; i < nbuf; i++ {
		b := &shmBuf{
			wl:   pool.CreateBuffer(int32(i*frameSize), int32(p.width), int32(p.height), int32(stride), format),
			data: data[i*frameSize : (i+1)*frameSize],
		}
		b.wl.SetListener(wl.BufferListener{
			Release: func(_ any, _ wl.Buffer) error {
				p.free = append(p.free, b)
				return nil
			},
		}, nil)
		p.free = append(p.free, b)
	}
	pool.Destroy()

	// Audio plays on its own goroutine so a blocking write never stalls dispatch.
	audioCh := make(chan []int16, 64)
	p.audioCh = audioCh
	var wg sync.WaitGroup
	if hasAudio {
		config := &alsa.Config{
			Channels:    2,
			Rate:        uint32(samplerate),
			PeriodSize:  1024,
			PeriodCount: 4,
			Format:      alsa.SNDRV_PCM_FORMAT_S16_LE,
		}

		pcm, err := alsa.PcmOpen(0, 0, alsa.PCM_OUT, config)
		if err != nil {
			fmt.Println("audio disabled:", err)
			mpg.SetAudioEnabled(false)
			hasAudio = false
		} else {
			defer pcm.Close()

			mpg.SetAudioFormat(mpeg.AudioS16)
			duration := float64(mpeg.SamplesPerFrame) / float64(samplerate)
			mpg.SetAudioLeadTime(time.Duration(duration * float64(time.Second)))

			mpg.SetAudioCallback(func(m *mpeg.MPEG, samples *mpeg.Samples) {
				if samples == nil {
					return
				}
				buf := make([]int16, len(samples.S16))
				copy(buf, samples.S16)
				select {
				case audioCh <- buf:
				default: // ring full, drop to avoid stalling decode
				}
			})

			wg.Add(1)
			go func() {
				defer wg.Done()
				for s := range audioCh {
					if _, err := pcm.Write(s); err != nil {
						fmt.Println(err)
					}
				}
			}()
		}
	}

	mpg.SetVideoCallback(func(m *mpeg.MPEG, frame *mpeg.Frame) {
		if frame == nil || len(p.free) == 0 {
			return
		}
		b := p.free[len(p.free)-1]
		p.free = p.free[:len(p.free)-1]

		// XBGR8888 matches image.RGBA (R,G,B,A); XRGB8888 needs R and B swapped.
		src := frame.RGBA().Pix
		if p.useXBGR {
			copy(b.data, src)
		} else {
			for i := 0; i < len(src); i += 4 {
				b.data[i+0] = src[i+2]
				b.data[i+1] = src[i+1]
				b.data[i+2] = src[i+0]
				b.data[i+3] = src[i+3]
			}
		}

		// Return an unconsumed previous frame so we keep the latest without leaking.
		if p.present != nil {
			p.free = append(p.free, p.present)
		}
		p.present = b
	})

	// Initial commit triggers the first configure, which kicks the render loop.
	p.surface.Commit()
	must(p.dpy.Flush())

	for !p.quit {
		if err := p.dpy.Dispatch(); err != nil {
			fmt.Println(err)
			break
		}
		if err := p.dpy.Flush(); err != nil {
			fmt.Println(err)
			break
		}
	}

	close(audioCh)
	wg.Wait()
}

// renderFrame decodes one tick, presents any new frame with feedback, and
// re-arms the frame callback so the compositor paces the next iteration.
func (p *player) renderFrame() {
	now := time.Now()
	if p.paused {
		p.last = now
	} else {
		tick := now.Sub(p.last).Seconds()
		p.last = now
		if p.framerate > 0 && tick > 1.0/p.framerate {
			tick = 1.0 / p.framerate
		}

		p.mpg.Decode(time.Duration(tick * float64(time.Second)))

		if p.mpg.HasEnded() {
			p.quit = true
			return
		}
	}

	if p.hasViewport && p.viewportDirty {
		p.viewport.SetDestination(int32(p.winW), int32(p.winH))
		p.viewportDirty = false
	}

	if p.present != nil {
		p.surface.Attach(p.present.wl, 0, 0)
		p.surface.DamageBuffer(0, 0, int32(p.width), int32(p.height))
		if p.hasPresent {
			fb := p.presentation.Feedback(p.surface)
			fb.SetListener(wp.PresentationFeedbackListener{
				Presented: p.onPresented,
				Discarded: p.onDiscarded,
			}, nil)
		}
		p.present = nil
	}

	cb := p.surface.Frame()
	cb.SetListener(wl.CallbackListener{
		Done: func(_ any, _ wl.Callback, _ uint32) error {
			p.renderFrame()
			return nil
		},
	}, nil)
	p.surface.Commit()
}

func (p *player) onPresented(_ any, _ wp.PresentationFeedback, tvSecHi, tvSecLo, tvNsec, refresh, seqHi, seqLo uint32, flags wp.PresentationFeedbackKind) error {
	ns := (uint64(tvSecHi)<<32|uint64(tvSecLo))*1e9 + uint64(tvNsec)
	if p.lastNs != 0 {
		p.accumNs += ns - p.lastNs
		p.accumCount++
	}
	p.lastNs = ns
	p.presented++

	if p.accumCount >= 120 {
		avg := float64(p.accumNs) / float64(p.accumCount) / 1e6
		fmt.Printf("presented %d (%.1f fps), discarded %d, refresh %dns\n", p.presented, 1000.0/avg, p.discarded, refresh)
		p.accumNs, p.accumCount = 0, 0
	}
	return nil
}

func (p *player) onDiscarded(_ any, _ wp.PresentationFeedback) error {
	p.discarded++
	return nil
}

// onKey handles a key press; codes are Linux evdev keycodes from wl_keyboard.
func (p *player) onKey(key uint32) {
	switch key {
	case 1, 16: // escape, q
		p.quit = true
	case 57, 25: // space, p
		p.paused = !p.paused
		p.last = time.Now()
	case 106: // right
		p.seek(3)
	case 105: // left
		p.seek(-3)
	case 33, 87: // f, f11
		if p.fullscreen {
			p.toplevel.UnsetFullscreen()
			p.fullscreen = false
		} else if p.hasOutput {
			p.toplevel.SetFullscreen(p.output)
			p.fullscreen = true
		}
	}
}

func (p *player) seek(delta float64) {
	t := p.mpg.Time().Seconds() + delta
	if t < 0 {
		t = 0
	}
	p.mpg.Seek(time.Duration(t*float64(time.Second)), false)

	// Drop audio queued from before the seek.
	for {
		select {
		case <-p.audioCh:
		default:
			p.last = time.Now()
			return
		}
	}
}

func must(err error) {
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
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
