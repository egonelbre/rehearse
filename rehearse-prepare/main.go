package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/exp/slices"
)

type flags struct {
	inputDir  string
	outputDir string
	gain      float64
	pan       float64

	skipTrackplay bool
	skipRehearse  bool
}

func main() {
	flags := flags{}
	flag.StringVar(&flags.inputDir, "in", "", "input directory")
	flag.StringVar(&flags.outputDir, "out", "", "output directory")
	flag.Float64Var(&flags.gain, "gain", 1, "gain adjustment for background tracks")
	flag.Float64Var(&flags.pan, "pan", 1, "pan for rehearsal track")
	flag.BoolVar(&flags.skipTrackplay, "skip-trackplay", false, "disable trackplay generation")
	flag.BoolVar(&flags.skipRehearse, "skip-rehearse", false, "disable rehearsal track generation")

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
		if file.IsDir() {
			continue
		}
		if audioExt[filepath.Ext(file.Name())] {
			tracks = append(tracks, filepath.Join(flags.inputDir, file.Name()))
		}
	}

	if !flags.skipTrackplay {
		_ = os.MkdirAll(filepath.Join(flags.outputDir, "trackplay"), 0755)
		// trackplay
		for _, track := range tracks {
			dest := filepath.Join(flags.outputDir, "trackplay", filepath.Base(track))
			dest = removeExt(dest) + ".mp3"

			cmd := exec.Command("ffmpeg", "-i", track, "-ac", "1", dest)
			cmd.Stderr = os.Stderr
			cmd.Stdout = os.Stdout
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("failed %q: %w", strings.Join(cmd.Args, " "), err)
			}
		}
		data, _ := json.MarshalIndent(createTrackplayIndex(tracks), "", "\t")
		indexPath := filepath.Join(flags.outputDir, "trackplay", "index.json")
		if err = os.WriteFile(indexPath, data, 0755); err != nil {
			return fmt.Errorf("failed to write index: %w", err)
		}
	}

	if !flags.skipRehearse {
		// individual rehearsal tracks
		_ = os.MkdirAll(filepath.Join(flags.outputDir, "rehearsal"), 0755)
		for target, track := range tracks {
			args := []string{"-y"}

			for _, track := range tracks {
				args = append(args, "-i", track)
			}

			// add inputs to -filter_complex
			amerge := ""
			for i := range tracks {
				amerge += fmt.Sprintf("[%d:a]", i)
			}
			amerge += fmt.Sprintf("amerge=inputs=%d", len(tracks))
			amerge += ",pan=stereo"
			left := ""
			right := ""
			for k := range tracks {
				pan := 1 - flags.pan
				gain := flags.gain / math.Log2(float64(len(tracks)))
				if k == target {
					pan = 1 - pan
					gain = 1
				}
				gain *= 0.5

				if pan < 1 {
					if left != "" {
						left += "+"
					}
					left += fmt.Sprintf("%.2f*c%d+%.2f*c%d", gain*(1-pan), 2*k, gain*(1-pan), 2*k+1)
				}
				if pan > 0 {
					if right != "" {
						right += "+"
					}
					right += fmt.Sprintf("%.2f*c%d+%.2f*c%d", gain*pan, 2*k, gain*pan, 2*k+1)
				}
			}
			amerge += "|c0=" + left + "|c1=" + right
			args = append(args, "-filter_complex", amerge)

			dest := filepath.Join(flags.outputDir, "rehearsal", filepath.Base(track))
			dest = removeExt(dest) + ".mp3"
			args = append(args, dest)

			cmd := exec.Command("ffmpeg", args...)
			cmd.Stderr = os.Stderr
			cmd.Stdout = os.Stdout
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("failed %q: %w", strings.Join(cmd.Args, " "), err)
			}
		}
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
			Name:      removeExt(filepath.Base(path)),
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
