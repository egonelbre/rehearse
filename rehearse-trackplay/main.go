package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

type flags struct {
	inputDir  string
	outputDir string
}

func main() {
	flags := flags{}
	flag.StringVar(&flags.inputDir, "in", "", "input directory")
	flag.StringVar(&flags.outputDir, "out", "", "combined output directory")

	flag.Parse()

	flagErr := combine(
		check(flags.inputDir != "", "-in must be specified"),
		check(flags.outputDir != "", "-out must be specified"),
	)
	if flagErr != nil {
		flag.Usage()
		os.Exit(1)
	}

	runErr := run(flags)
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

var audioExt = map[string]bool{
	".aiff": true,
	".mp3":  true,
	".wav":  true,
}

func run(flags flags) error {
	files, err := os.ReadDir(flags.inputDir)
	if err != nil {
		return fmt.Errorf("failed to read input directory %q: %w", flags.inputDir, err)
	}

	tracks := []string{}
	for _, file := range files {
		if file.IsDir() || strings.HasPrefix(file.Name(), ".") {
			continue
		}
		if audioExt[filepath.Ext(file.Name())] {
			tracks = append(tracks, filepath.Join(flags.inputDir, file.Name()))
		}
	}

	_ = os.MkdirAll(filepath.Join(flags.outputDir), 0755)

	for _, track := range tracks {
		dest := filepath.Join(flags.outputDir, strings.TrimSpace(filepath.Base(track)))
		dest = removeExt(dest) + ".mp3"

		cmd := exec.Command("ffmpeg", "-i", track, "-ac", "1", dest)
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed %q: %w", strings.Join(cmd.Args, " "), err)
		}
	}

	data, _ := json.MarshalIndent(createTrackplayIndex(tracks), "", "\t")
	indexPath := filepath.Join(flags.outputDir, "index.json")
	if err = os.WriteFile(indexPath, data, 0755); err != nil {
		return fmt.Errorf("failed to write index: %w", err)
	}

	return nil
}

type trackplayIndex struct {
	Tracks []trackplayTrack `json:"tracks"`
}

type trackplayTrack struct {
	Name      string  `json:"name"`
	StereoPan float64 `json:"stereoPan"`
	Gain      float64 `json:"gain"`
}

func createTrackplayIndex(tracks []string) trackplayIndex {
	order := []string{
		"Solo",
		"Soprano",
		"Alto",
		"Tenor",
		"Baritone",
		"Bass",
	}

	index := trackplayIndex{}

	add := func(path string) {
		index.Tracks = append(index.Tracks, trackplayTrack{
			Name:      removeExt(strings.TrimSpace(filepath.Base(path))),
			StereoPan: -0.8,
			Gain:      0.5,
		})
	}

	left := slices.Clone(tracks)
	slices.Sort(left)
	for _, item := range order {
		off := 0
		for i, path := range left {
			if strings.Contains(path, item) {
				add(path)
				left = slices.Delete(left, i+off, i+off+1)
				off--
			}
		}
	}

	for _, p := range left {
		add(p)
	}

	return index
}

func removeExt(p string) string {
	return p[:len(p)-len(filepath.Ext(p))]
}

func rehearsal(flags flags, tracks []string) error {
	return nil
}

func check(v bool, format string, args ...interface{}) error {
	if !v {
		return fmt.Errorf(format, args...)
	}
	return nil
}

func combine(errs ...error) error {
	xs := errs[:0]
	for _, e := range errs {
		if e != nil {
			xs = append(xs, e)
		}
	}
	if len(xs) == 0 {
		return nil
	}
	return fmt.Errorf("%v", errs)
}

func parallel(fns ...func()) {
	var wg sync.WaitGroup
	wg.Add(len(fns))
	for _, fn := range fns {
		fn := fn
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	wg.Wait()
}
