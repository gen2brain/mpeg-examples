package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zyko0/go-sdl3/sdl"
	"github.com/jfbus/httprs"

	"github.com/gen2brain/mpeg"
)

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

		stream = devices[0].OpenAudioDeviceStream(&spec, 0)
		defer stream.Destroy()

		err = stream.ResumeDevice()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		duration := float64(mpeg.SamplesPerFrame) / float64(samplerate)
		mpg.SetAudioLeadTime(time.Duration(duration * float64(time.Second)))

		mpg.SetAudioCallback(func(m *mpeg.MPEG, samples *mpeg.Samples) {
			if samples == nil {
				return
			}

			err = stream.PutData(samples.Bytes())
			if err != nil {
				fmt.Println(err)
			}
		})
	}

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

		mpg.SetVideoCallback(func(m *mpeg.MPEG, frame *mpeg.Frame) {
			if frame == nil {
				return
			}

			err = texture.UpdateYUV(nil, frame.Y.Data, int32(frame.Y.Width), frame.Cb.Data, int32(frame.Cb.Width), frame.Cr.Data, int32(frame.Cr.Width))
			if err != nil {
				fmt.Println(err)
			}
		})
	}

	var pause bool
	var seekTo, lastTime, currentTime, elapsedTime float64

	running := true
	for running {
		seekTo = -1

		var ev sdl.Event
		for sdl.PollEvent(&ev) {
			switch ev.Type {
			case sdl.EVENT_QUIT:
				running = false
			case sdl.EVENT_MOUSE_BUTTON_DOWN:
				if ev.MouseButtonEvent().Clicks == 2 {
					err = toggleFullscreen(renderer)
					if err != nil {
						fmt.Println(err)
					}
				}
			case sdl.EVENT_KEY_DOWN:
				if ev.KeyboardEvent().Key == sdl.K_ESCAPE || ev.KeyboardEvent().Key == sdl.K_Q {
					running = false
				} else if ev.KeyboardEvent().Key == sdl.K_SPACE || ev.KeyboardEvent().Key == sdl.K_P {
					pause = !pause
				} else if ev.KeyboardEvent().Key == sdl.K_F || ev.KeyboardEvent().Key == sdl.K_F11 {
					err = toggleFullscreen(renderer)
					if err != nil {
						fmt.Println(err)
					}
				} else if ev.KeyboardEvent().Key == sdl.K_RIGHT {
					seekTo = mpg.Time().Seconds() + 3
				} else if ev.KeyboardEvent().Key == sdl.K_LEFT {
					seekTo = mpg.Time().Seconds() - 3
				}
			}
		}

		if !pause {
			currentTime = float64(sdl.Ticks()) / 1000
			elapsedTime = currentTime - lastTime
			if elapsedTime > 1.0/framerate {
				elapsedTime = 1.0 / framerate
			}
			lastTime = currentTime

			if seekTo != -1 {
				if hasAudio {
					stream.Clear()
				}
				mpg.Seek(time.Duration(seekTo*float64(time.Second)), false)
			} else {
				mpg.Decode(time.Duration(elapsedTime * float64(time.Second)))
			}
		}

		if mpg.HasEnded() {
			running = false
		}

		if hasVideo && seekTo == -1 {
			err = renderer.Clear()
			if err != nil {
				fmt.Println(err)
			}

			err = renderer.RenderTexture(texture, nil, nil)
			if err != nil {
				fmt.Println(err)
			}

			renderer.Present()
		}
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
