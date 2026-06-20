package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"
	"github.com/jfbus/httprs"

	"github.com/gen2brain/mpeg"
)

// frameBuf is a CPU-side copy of one decoded video frame, handed from the
// decode goroutine to the render (main) thread. The decoder reuses its own
// frame buffers, so the planes are copied out.
type frameBuf struct {
	y, cb, cr        []byte
	yStride, cStride int
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

	hasVideo := mpg.NumVideoStreams() > 0
	hasAudio := mpg.NumAudioStreams() > 0
	mpg.SetVideoEnabled(hasVideo)
	mpg.SetAudioEnabled(hasAudio)

	framerate := mpg.Framerate()
	samplerate := mpg.Samplerate()

	var flags sdl.InitFlags
	if hasVideo {
		flags = sdl.INIT_VIDEO
	}
	if hasAudio {
		flags |= sdl.INIT_AUDIO
	}

	err = sdl.LoadLibrary(sdl.Path())
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	err = sdl.Init(flags)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer sdl.Quit()

	var texture *sdl.Texture
	var renderer *sdl.Renderer
	var stream *sdl.AudioStream

	if hasAudio {
		spec := sdl.AudioSpec{
			Freq:     int32(samplerate),
			Format:   sdl.AUDIO_F32,
			Channels: 2,
		}

		sdl.SetHint(sdl.HINT_AUDIO_DEVICE_SAMPLE_FRAMES, "1152")

		devices, err := sdl.GetAudioPlaybackDevices()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if len(devices) == 0 {
			fmt.Println("no audio playback device")
			os.Exit(1)
		}

		stream = devices[0].OpenAudioDeviceStream(&spec, 0)
		defer stream.Destroy()

		err = stream.ResumeDevice()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		duration := float64(mpeg.SamplesPerFrame) / float64(samplerate)
		mpg.SetAudioLeadTime(time.Duration(duration * float64(time.Second)))

		// Audio is queued from the decode goroutine; SDL's audio stream is
		// thread-safe.
		mpg.SetAudioCallback(func(m *mpeg.MPEG, samples *mpeg.Samples) {
			if samples == nil {
				return
			}
			if err := stream.PutData(samples.Bytes()); err != nil {
				fmt.Println(err)
			}
		})
	}

	// Decode-ahead pipeline: the decoder runs on its own goroutine and passes
	// CPU frame copies to the main thread, which owns all SDL rendering. This
	// overlaps decoding the next frame with uploading and presenting (the vsync
	// wait of) the current one.
	ready := make(chan *frameBuf, 1)
	free := make(chan *frameBuf, 3)
	quit := make(chan struct{})

	if hasVideo {
		width := mpg.Width()
		height := mpg.Height()

		window, err := sdl.CreateWindow(filepath.Base(os.Args[1]), width, height, sdl.WINDOW_RESIZABLE|sdl.WINDOW_OPENGL)
		if err != nil {
			fmt.Println(err)
		}
		defer window.Destroy()

		window.SetPosition(sdl.WINDOWPOS_CENTERED, sdl.WINDOWPOS_CENTERED)

		renderer, err = window.CreateRenderer("opengl")
		if err != nil {
			fmt.Println(err)
		}
		defer renderer.Destroy()

		err = renderer.SetVSync(1)
		if err != nil {
			fmt.Println(err)
		}

		texture, err = renderer.CreateTexture(sdl.PIXELFORMAT_YV12, sdl.TEXTUREACCESS_STREAMING, width, height)
		if err != nil {
			fmt.Println(err)
		}
		defer texture.Destroy()

		for i := 0; i < cap(free); i++ {
			free <- &frameBuf{}
		}

		mpg.SetVideoCallback(func(m *mpeg.MPEG, frame *mpeg.Frame) {
			if frame == nil {
				return
			}

			var fb *frameBuf
			select {
			case fb = <-free:
			case <-quit:
				return
			}

			fb.y = append(fb.y[:0], frame.Y.Data...)
			fb.cb = append(fb.cb[:0], frame.Cb.Data...)
			fb.cr = append(fb.cr[:0], frame.Cr.Data...)
			fb.yStride = frame.Y.Width
			fb.cStride = frame.Cb.Width

			select {
			case ready <- fb:
			case <-quit:
			}
		})
	}

	// Playback control shared with the decode goroutine. Seeks are relative so
	// the main thread never touches the decoder.
	var mu sync.Mutex
	pause := false
	seekDelta := 0.0
	ended := false

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		last := time.Now()
		for {
			select {
			case <-quit:
				return
			default:
			}

			mu.Lock()
			p := pause
			s := seekDelta
			seekDelta = 0
			mu.Unlock()

			if s != 0 {
				if hasAudio {
					stream.Clear()
				}
				mpg.Seek(time.Duration((mpg.Time().Seconds()+s)*float64(time.Second)), false)
				last = time.Now()
				continue
			}

			if p {
				last = time.Now()
				time.Sleep(10 * time.Millisecond)
				continue
			}

			now := time.Now()
			elapsed := now.Sub(last).Seconds()
			last = now
			if framerate > 0 && elapsed > 1.0/framerate {
				elapsed = 1.0 / framerate
			}

			mpg.Decode(time.Duration(elapsed * float64(time.Second)))

			if mpg.HasEnded() {
				mu.Lock()
				ended = true
				mu.Unlock()
				return
			}

			time.Sleep(time.Millisecond)
		}
	}()

	running := true
	for running {
		var ev sdl.Event
		for sdl.PollEvent(&ev) {
			switch ev.Type {
			case sdl.EVENT_QUIT:
				running = false
			case sdl.EVENT_MOUSE_BUTTON_DOWN:
				if ev.MouseButtonEvent().Clicks == 2 {
					if err := toggleFullscreen(renderer); err != nil {
						fmt.Println(err)
					}
				}
			case sdl.EVENT_KEY_DOWN:
				switch ev.KeyboardEvent().Key {
				case sdl.K_ESCAPE, sdl.K_Q:
					running = false
				case sdl.K_SPACE, sdl.K_P:
					mu.Lock()
					pause = !pause
					mu.Unlock()
				case sdl.K_F, sdl.K_F11:
					if err := toggleFullscreen(renderer); err != nil {
						fmt.Println(err)
					}
				case sdl.K_RIGHT:
					mu.Lock()
					seekDelta = 3
					mu.Unlock()
				case sdl.K_LEFT:
					mu.Lock()
					seekDelta = -3
					mu.Unlock()
				}
			}
		}

		if hasVideo {
			// Upload the most recent decoded frame, if one is ready.
			select {
			case fb := <-ready:
				if err := texture.UpdateYUV(nil, fb.y, int32(fb.yStride), fb.cb, int32(fb.cStride), fb.cr, int32(fb.cStride)); err != nil {
					fmt.Println(err)
				}
				free <- fb
			default:
			}

			if err := renderer.Clear(); err != nil {
				fmt.Println(err)
			}
			if err := renderer.RenderTexture(texture, nil, nil); err != nil {
				fmt.Println(err)
			}
			renderer.Present()
		} else {
			time.Sleep(time.Millisecond)
		}

		mu.Lock()
		e := ended
		mu.Unlock()
		if e && len(ready) == 0 {
			running = false
		}
	}

	close(quit)
	wg.Wait()
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

func toggleFullscreen(renderer *sdl.Renderer) error {
	window, err := renderer.Window()
	if err != nil {
		return err
	}

	isFullscreen := window.Flags()&sdl.WINDOW_FULLSCREEN != 0
	if isFullscreen {
		err := window.SetFullscreen(false)
		if err != nil {
			return err
		}
		err = sdl.ShowCursor()
		if err != nil {
			return err
		}
	} else {
		err := window.SetFullscreen(true)
		if err != nil {
			return err
		}
		err = sdl.HideCursor()
		if err != nil {
			return err
		}
	}

	return nil
}
