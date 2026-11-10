package video

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"remotedesk/internal/capture"
	"remotedesk/internal/sysutil"
)

// Encoder, BGRA kareleri stdin'den alıp H.264 kareleri üreten bir ffmpeg sürecidir.
type Encoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	bw     *bufio.Writer
	frames chan Frame
	done   chan struct{}
	once   sync.Once
	job    *sysutil.Job

	inW, inH int

	errMu   sync.Mutex
	errTail []string
}

// Start, ffmpeg'i başlatır. inW×inH giriş boyutu, outW×outH kodlanan boyuttur (çift sayı olmalı).
func Start(ctx context.Context, bin, enc string, inW, inH, outW, outH, fps, bitrate int) (*Encoder, error) {
	vf := "format=nv12"
	if outW != inW || outH != inH {
		vf = fmt.Sprintf("scale=%d:%d:flags=bilinear,format=nv12", outW, outH)
	}
	args := []string{"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "bgra", "-video_size", fmt.Sprintf("%dx%d", inW, inH),
		"-framerate", strconv.Itoa(fps), "-i", "pipe:0",
		"-vf", vf, "-c:v", enc}
	args = append(args, encoderArgs(enc, bitrate)...)
	args = append(args, "-flush_packets", "1", "-f", "h264", "pipe:1")

	cmd := exec.CommandContext(ctx, bin, args...)
	sysutil.HideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	job, _ := sysutil.KillWithParent(cmd.Process)

	e := &Encoder{
		cmd: cmd, stdin: stdin, bw: bufio.NewWriterSize(stdin, 1<<20),
		frames: make(chan Frame, 8), done: make(chan struct{}), job: job,
		inW: inW, inH: inH,
	}
	go e.readStderr(stderr)
	go e.parse(stdout)
	return e, nil
}

// Frames, kodlanmış kareleri verir; ffmpeg kapanınca kapanır.
func (e *Encoder) Frames() <-chan Frame { return e.frames }

// WriteFrame bir ekran karesini kodlayıcıya gönderir.
func (e *Encoder) WriteFrame(f *capture.Frame) error {
	if f.W != e.inW || f.H != e.inH {
		return fmt.Errorf("kare boyutu değişti (%dx%d)", f.W, f.H)
	}
	row := f.W * 4
	if f.Stride == row {
		_, err := e.stdin.Write(f.Pix[:row*f.H])
		return err
	}
	for y := 0; y < f.H; y++ {
		if _, err := e.bw.Write(f.Pix[y*f.Stride : y*f.Stride+row]); err != nil {
			return err
		}
	}
	return e.bw.Flush()
}

// ErrText, ffmpeg'in son hata satırlarını döner.
func (e *Encoder) ErrText() string {
	e.errMu.Lock()
	defer e.errMu.Unlock()
	return strings.Join(e.errTail, " | ")
}

func (e *Encoder) Close() {
	e.once.Do(func() {
		close(e.done)
		e.stdin.Close()
		if e.cmd.Process != nil {
			e.cmd.Process.Kill()
		}
	})
}

func (e *Encoder) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		log.Printf("[ffmpeg] %s", line)
		e.errMu.Lock()
		e.errTail = append(e.errTail, line)
		if len(e.errTail) > 3 {
			e.errTail = e.errTail[1:]
		}
		e.errMu.Unlock()
	}
}

func (e *Encoder) emit(f Frame) {
	select {
	case e.frames <- f:
	case <-e.done:
	}
}

func (e *Encoder) parse(r io.Reader) {
	defer func() {
		close(e.frames)
		e.cmd.Wait()
		e.job.Close()
	}()
	chunks := make(chan []byte, 16)
	go func() {
		defer close(chunks)
		buf := make([]byte, 256<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				c := make([]byte, n)
				copy(c, buf[:n])
				select {
				case chunks <- c:
				case <-e.done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	var p auParser
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				p.flush(e.emit)
				return
			}
			p.feed(c, e.emit)
			timer.Reset(4 * time.Millisecond)
		case <-timer.C:
			p.flush(e.emit)
		case <-e.done:
			return
		}
	}
}
