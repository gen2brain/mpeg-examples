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

	rl "github.com/gen2brain/raylib-go/raylib"
	"github.com/jfbus/httprs"

	"github.com/gen2brain/mpeg"
)

// audioRing is a thread-safe FIFO of interleaved float32 samples. The mpeg
// audio callback writes decoded samples from the decode goroutine; raylib's
// audio thread drains them through the stream callback. This decouples decode
// pacing from the device clock, the same way SDL's QueueAudio does.
type audioRing struct {
	mu  sync.Mutex
	buf []float32
}

func (a *audioRing) write(s []float32) {
	a.mu.Lock()
	a.buf = append(a.buf, s...)
	a.mu.Unlock()
}

func (a *audioRing) read(out []float32) {
	a.mu.Lock()
	n := copy(out, a.buf)
	a.buf = a.buf[:copy(a.buf, a.buf[n:])]
	a.mu.Unlock()
	for i := n; i < len(out); i++ {
		out[i] = 0
	}
}

func (a *audioRing) clear() {
	a.mu.Lock()
	a.buf = a.buf[:0]
	a.mu.Unlock()
}

// frameBuf is a CPU-side copy of one decoded video frame, handed from the
// decode goroutine to the render (main) thread. The decoder reuses its own
// RGBA buffer, so the pixels are copied out.
type frameBuf struct {
	rgba []byte
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

	width := int32(mpg.Width())
	height := int32(mpg.Height())
	framerate := mpg.Framerate()
	samplerate := mpg.Samplerate()

	var stream rl.AudioStream
	var texture rl.Texture2D
	var target rl.RenderTexture2D

	rl.SetConfigFlags(rl.FlagVsyncHint | rl.FlagWindowResizable)
	rl.InitWindow(width, height, filepath.Base(os.Args[1]))
	defer rl.CloseWindow()

	ring := &audioRing{}

	if hasAudio {
		mpg.SetAudioFormat(mpeg.AudioF32N)

		rl.InitAudioDevice()
		defer rl.CloseAudioDevice()

		rl.SetAudioStreamBufferSizeDefault(mpeg.SamplesPerFrame * 2)

		stream = rl.LoadAudioStream(uint32(samplerate), 32, 2)
		defer rl.UnloadAudioStream(stream)

		rl.SetAudioStreamCallback(stream, func(data []float32, frames int) {
			ring.read(data)
		})
		rl.PlayAudioStream(stream)

		duration := float64(mpeg.SamplesPerFrame*2) / float64(samplerate)
		mpg.SetAudioLeadTime(time.Duration(duration * float64(time.Second)))

		mpg.SetAudioCallback(func(m *mpeg.MPEG, samples *mpeg.Samples) {
			if samples == nil {
				return
			}

			ring.write(samples.Interleaved)
		})
	}

	// Decode-ahead pipeline: the decoder runs on its own goroutine and passes
	// CPU frame copies to the main thread, which owns all raylib rendering. This
	// overlaps decoding (and the YCbCr to RGBA conversion of) the next frame with
	// uploading and presenting (the vsync wait of) the current one.
	ready := make(chan *frameBuf, 1)
	free := make(chan *frameBuf, 3)
	quit := make(chan struct{})

	if hasVideo {
		imFrame := &rl.Image{}
		imFrame.Width = width
		imFrame.Height = height
		imFrame.Format = rl.UncompressedR8g8b8a8
		imFrame.Mipmaps = 1
		defer rl.UnloadImage(imFrame)

		texture = rl.LoadTextureFromImage(imFrame)
		defer rl.UnloadTexture(texture)

		target = rl.LoadRenderTexture(width, height)
		defer rl.UnloadRenderTexture(target)

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

			fb.rgba = append(fb.rgba[:0], frame.RGBA().Pix...)

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
					ring.clear()
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
		if rl.IsKeyPressed(rl.KeyQ) || rl.WindowShouldClose() {
			running = false
		} else if rl.IsKeyPressed(rl.KeySpace) || rl.IsKeyPressed(rl.KeyP) {
			mu.Lock()
			pause = !pause
			p := pause
			mu.Unlock()
			if hasAudio {
				if p {
					rl.PauseAudioStream(stream)
				} else {
					rl.ResumeAudioStream(stream)
				}
			}
		} else if rl.IsKeyPressed(rl.KeyF) || rl.IsKeyPressed(rl.KeyF11) {
			if hasVideo {
				rl.ToggleBorderlessWindowed()
			}
		} else if rl.IsKeyPressed(rl.KeyRight) {
			mu.Lock()
			seekDelta = 3
			mu.Unlock()
		} else if rl.IsKeyPressed(rl.KeyLeft) {
			mu.Lock()
			seekDelta = -3
			mu.Unlock()
		}

		if hasVideo {
			// Upload the most recent decoded frame, if one is ready.
			select {
			case fb := <-ready:
				rl.UpdateTexture(texture, fb.rgba)
				free <- fb
			default:
			}

			rl.BeginDrawing()
			rl.ClearBackground(rl.White)

			rl.BeginTextureMode(target)
			rl.DrawTexture(texture, 0, 0, rl.White)
			rl.EndTextureMode()

			rl.DrawTexturePro(
				target.Texture,
				rl.NewRectangle(0, 0, float32(target.Texture.Width), float32(-target.Texture.Height)),
				rl.NewRectangle(0, 0, float32(rl.GetScreenWidth()), float32(rl.GetScreenHeight())),
				rl.NewVector2(0, 0),
				0,
				rl.White,
			)

			rl.EndDrawing()
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
