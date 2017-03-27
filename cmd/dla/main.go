package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/fatih/color"
	"github.com/fsouza/go-dockerclient"
)

type flgs struct {
	follow bool
	tail   string
}

var flags = flgs{}

func init() {
	flag.BoolVar(&flags.follow, "f", false, "Follow log output")
	flag.StringVar(&flags.tail, "t", "", "Tail size of log output")
}

const (
	swarmServiceNameKey = "com.docker.swarm.service.name"
	swarmTaskNameKey    = "com.docker.swarm.task.name"
)

func main() {
	flag.Parse()

	client, err := docker.NewClientFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to setup connection to docker: %s\n", err)
		os.Exit(1)
	}

	conts, err := getContainersByNames(client, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error retrieving container information: %s\n", err)
		os.Exit(1)
	} else if len(conts) <= 0 {
		fmt.Println("No services meet the criteria")
		return
	}

	logContainers(client, conts)
}

func getContainersByNames(client *docker.Client, names []string) ([]docker.APIContainers, error) {
	conts := make([]docker.APIContainers, 0, len(names))

	switch len(names) {
	case 0:
		return getAllContainers(client)

	default:
		type contr struct {
			conts []docker.APIContainers
			err   error
		}
		ch := make(chan contr, len(names))
		wg := sync.WaitGroup{}
		wg.Add(len(names))
		for _, name := range names {
			go func(name string) {
				defer wg.Done()
				iconts, err := getContainerForName(client, name)
				ch <- contr{
					conts: iconts,
					err:   err,
				}
			}(name)
		}

		wg.Wait()
		close(ch)

		for contr := range ch {
			if contr.err != nil {
				return nil, contr.err
			}
			conts = append(conts, contr.conts...)
		}
	}

	// dedupe containers
	found := map[string]struct{}{}
	out := conts[:0]
	for _, cont := range conts {
		if _, ok := found[cont.ID]; !ok {
			out = append(out, cont)
		}
	}

	return out, nil
}

func getAllContainers(client *docker.Client) ([]docker.APIContainers, error) {
	return client.ListContainers(docker.ListContainersOptions{})
}

func getContainerForName(client *docker.Client, name string) ([]docker.APIContainers, error) {
	return client.ListContainers(docker.ListContainersOptions{
		Filters: map[string][]string{
			"label": []string{swarmServiceNameKey + "=" + name},
		},
	})
}

const (
	postFix = " | "
)

func getTags(conts []docker.APIContainers) []string {
	tags := make([]string, 0, len(conts))
	for _, cont := range conts {
		tags = append(tags, cont.Labels[swarmTaskNameKey])
	}
	return tags
}

func logContainers(client *docker.Client, conts []docker.APIContainers) {
	if client == nil || len(conts) <= 0 {
		return
	}

	tagFmt := tagConfig(getTags(conts), " | ")

	wOut := NewFanInWriter(os.Stdout)
	wErr := NewFanInWriter(os.Stderr)

	wg := sync.WaitGroup{}
	wg.Add(len(conts))

	for _, cont := range conts {
		name := cont.Labels[swarmTaskNameKey]
		tag := tagFmt(name)
		go func(cont docker.APIContainers) {
			defer wg.Done()
			err := client.Logs(docker.LogsOptions{
				Container:    cont.ID,
				Stdout:       true,
				OutputStream: LineWriter(wOut, tag, nil),
				Stderr:       true,
				ErrorStream:  LineWriter(wErr, tag, color.New(color.FgHiRed)),
				Follow:       flags.follow,
				Tail:         flags.tail,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Logger failed for %s: %s\n", name, err)
				os.Exit(1)
			}

			fmt.Printf("Stream %s exited.\n", name)
		}(cont)
	}

	wg.Wait()
}

var colors = []*color.Color{
	color.New(color.FgHiRed),
	color.New(color.FgHiGreen),
	color.New(color.FgHiYellow),
	color.New(color.FgHiBlue),
	color.New(color.FgHiMagenta),
	color.New(color.FgHiCyan),
	color.New(color.FgRed),
	color.New(color.FgGreen),
	color.New(color.FgYellow),
	color.New(color.FgBlue),
	color.New(color.FgMagenta),
	color.New(color.FgCyan),
}

func tagConfig(tags []string, postFix string) func(string) []byte {
	sort.Strings(tags)

	var tagLength int
	for i := range tags {
		if l := len(tags[i]); l > tagLength {
			tagLength = l
		}
	}

	cm := map[string][]byte{}
	for i, tag := range tags {
		fmtTag := tag
		fmtTag += strings.Repeat(" ", tagLength-len(tag))
		fmtTag += postFix
		cm[tag] = []byte(colors[i%len(colors)].Sprint(fmtTag))
	}

	return func(tag string) []byte {
		return cm[tag]
	}
}

func LineWriter(w io.Writer, tag []byte, color *color.Color) io.Writer {
	r, in := io.Pipe()

	go func() {
		scan := bufio.NewScanner(r)
		for scan.Scan() {
			var logLine []byte
			if color != nil {
				logLine = []byte(color.Sprint(scan.Bytes()))
			} else {
				logLine = scan.Bytes()
			}
			if _, err := fullWrite(w, append(tag, append(logLine, []byte("\n")...)...)); err != nil {
				fmt.Printf("Error attempting to write to dest: %s\n", err)
				break
			}
		}
		if scanErr := scan.Err(); scanErr != nil {
			fmt.Printf("Error recieving write from source: %s\n", scanErr)
		}
	}()

	return in
}

func scanLinesKeepCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// We have a full newline-terminated line.
		return i + 1, data[0 : i+1], nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), data, nil
	}
	// Request more data.
	return 0, nil, nil
}

type FanInWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func NewFanInWriter(w io.Writer) *FanInWriter {
	if w == nil {
		return nil
	}

	return &FanInWriter{
		out: w,
	}
}

func (fiw *FanInWriter) Write(b []byte) (n int, err error) {
	fiw.mu.Lock()
	n, err = fullWrite(fiw.out, b)
	fiw.mu.Unlock()
	return
}

func fullWrite(w io.Writer, b []byte) (n int, err error) {
	for n < len(b) {
		lw, err := w.Write(b[n:])
		n += lw
		if err != nil {
			return n, err
		}
	}

	return n, nil
}
