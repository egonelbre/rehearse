package main

import (
	"bytes"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/go-audio/aiff"
	"github.com/go-audio/wav"
)

type flags struct {
	parallel int

	inputDir  string
	outputDir string
	gain      float64
	pan       float64

	metronomeGain float64

	onlyRehearsal  bool
	onlyIndividual bool
	onlyCombined   bool

	selectTracks string
	ignoreTracks string
}

func main() {
	flags := flags{}
	flag.IntVar(&flags.parallel, "p", 8, "parallelization")

	flag.StringVar(&flags.inputDir, "in", "", "input directory")
	flag.StringVar(&flags.outputDir, "out", "", "output directory")
	flag.Float64Var(&flags.gain, "gain", 1, "gain adjustment for background tracks")
	flag.Float64Var(&flags.pan, "pan", 1, "pan for rehearsal track")
	flag.Float64Var(&flags.metronomeGain, "metronome", 0.6, "metronome gain")

	flag.BoolVar(&flags.onlyRehearsal, "only-rehearsal", false, "only output rehearsal")
	flag.BoolVar(&flags.onlyIndividual, "only-individual", false, "only output individual")
	flag.BoolVar(&flags.onlyCombined, "only-combined", false, "only output combined")

	flag.StringVar(&flags.selectTracks, "select", "", "only create output for the specified tracks")
	flag.StringVar(&flags.ignoreTracks, "ignore", "", "ignore specified track files")

	flag.Parse()

	flagErr := combine(
		check(flags.inputDir != "", "-in must be specified"),
		check(flags.outputDir != "", "-out must be specified"),
	)
	if flagErr != nil {
		flag.Usage()
		os.Exit(1)
	}

	if !(flags.onlyRehearsal || flags.onlyIndividual || flags.onlyCombined) {
		flags.onlyRehearsal = true
		flags.onlyIndividual = true
		flags.onlyCombined = true
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

var rxMetronome = regexp.MustCompile(`(?i)metronoo?me`)

func run(flags flags) error {
	files, err := os.ReadDir(flags.inputDir)
	if err != nil {
		return fmt.Errorf("failed to read input directory %q: %w", flags.inputDir, err)
	}

	rxIgnore := regexp.MustCompile("(?i)" + flags.ignoreTracks)

	var tracks Tracks
	for _, file := range files {
		if file.IsDir() || strings.HasPrefix(file.Name(), ".") {
			continue
		}
		if !audioExt[filepath.Ext(file.Name())] {
			continue
		}

		infile := filepath.Join(flags.inputDir, file.Name())

		if flags.ignoreTracks != "" && rxIgnore.MatchString(file.Name()) {
			continue
		}

		if rxMetronome.MatchString(file.Name()) {
			if tracks.Metronome.Path != "" {
				return fmt.Errorf("multiple metronome tracks, found %v and %v", tracks.Metronome, infile)
			}
			tracks.Metronome = Track{
				Path:     infile,
				Channels: readAudioChannelCount(infile),
			}
			continue
		}

		tracks.Parts = append(tracks.Parts, Track{
			Path:     infile,
			Channels: readAudioChannelCount(infile),
		})
	}

	group := new(errgroup.Group)
	if flags.parallel > 0 {
		group.SetLimit(flags.parallel)
	}

	if flags.onlyRehearsal {
		rehearsalTracks(group, filepath.Join(flags.outputDir, "Rehearse"), tracks, flags)
	}
	if flags.onlyIndividual {
		individualTracks(group, filepath.Join(flags.outputDir, "Individual"), tracks, flags)
	}
	if flags.onlyCombined {
		combinedTrack(group, flags.outputDir, tracks, flags)
	}
	group.Wait()

	return nil
}

type Tracks struct {
	Metronome Track
	Parts     []Track
}

type Track struct {
	Path     string
	Channels int
}

func rehearsalTracks(group *errgroup.Group, outdir string, tracks Tracks, flags flags) error {
	_ = os.MkdirAll(outdir, 0755)

	rxOnly := regexp.MustCompile("(?i)" + flags.selectTracks)

	for _, track := range tracks.Parts {
		if flags.selectTracks != "" && !rxOnly.MatchString(track.Path) {
			continue
		}

		group.Go(func() error {
			err := rehearsalTrack(outdir, tracks, flags, track)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Track %q failed: %v\n", track, err)
			}
			return nil
		})
	}

	return nil
}

func rehearsalTrack(outdir string, tracks Tracks, flags flags, track Track) error {
	args := []string{"-y"}

	inputCount := 0
	for _, track := range tracks.Parts {
		args = append(args, "-i", track.Path)
		inputCount++
	}
	if tracks.Metronome.Path != "" {
		args = append(args, "-i", tracks.Metronome.Path)
		inputCount++
	}

	// add inputs to -filter_complex
	amerge := ""
	for i := range inputCount {
		amerge += fmt.Sprintf("[%d:a]", i)
	}
	amerge += fmt.Sprintf(" amerge=inputs=%d,pan=stereo", inputCount)

	left := []string{}
	right := []string{}

	mergedChannelIndex := 0
	for _, target := range tracks.Parts {
		pan := 1 - flags.pan
		gain := flags.gain / math.Log2(float64(len(tracks.Parts)))
		if target == track {
			pan = 1 - pan
			gain = 1
		}

		gain /= float64(target.Channels)

		if pan < 1 {
			for i := range target.Channels {
				left = append(left, fmt.Sprintf("%.2f*c%d", gain*(1-pan), mergedChannelIndex+i))
			}
		}
		if pan > 0 {
			for i := range target.Channels {
				right = append(right, fmt.Sprintf("%.2f*c%d", gain*pan, mergedChannelIndex+i))
			}
		}

		mergedChannelIndex += target.Channels
	}

	if tracks.Metronome.Path != "" {
		metronome := []string{}
		gain := flags.metronomeGain / float64(tracks.Metronome.Channels)
		for i := range tracks.Metronome.Channels {
			metronome = append(metronome, fmt.Sprintf("%.2f*c%d", gain, mergedChannelIndex+i))
		}

		left = append(left, metronome...)
		right = append(right, metronome...)

		mergedChannelIndex += tracks.Metronome.Channels
	}

	amerge += "|c0=" + strings.Join(left, "+") + "|c1=" + strings.Join(right, "+")
	amerge += ",loudnorm"
	args = append(args, "-filter_complex", amerge)

	dest := filepath.Join(outdir, strings.TrimSpace(filepath.Base(track.Path)))
	dest = removeExt(dest) + ".mp3"
	args = append(args, dest)

	var buffer bytes.Buffer
	fmt.Fprint(&buffer, "$ ffmpeg")
	for _, arg := range args {
		fmt.Fprintf(&buffer, " %q", arg)
	}
	fmt.Fprintln(&buffer)

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = &buffer
	cmd.Stdout = &buffer
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed %q: %w", strings.Join(cmd.Args, " "), err)
	}

	fmt.Println(buffer.String())

	return nil
}

func individualTracks(group *errgroup.Group, outdir string, tracks Tracks, flags flags) error {
	_ = os.MkdirAll(outdir, 0755)

	rxOnly := regexp.MustCompile("(?i)" + flags.selectTracks)

	for _, track := range tracks.Parts {
		if flags.selectTracks != "" && !rxOnly.MatchString(track.Path) {
			continue
		}

		group.Go(func() error {
			err := individualTrack(outdir, tracks, flags, track)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Track %q failed: %v\n", track, err)
			}
			return nil
		})
	}

	return nil
}

func individualTrack(outdir string, tracks Tracks, flags flags, track Track) error {
	args := []string{"-y"}

	inputCount := 1
	args = append(args, "-i", track.Path)
	if tracks.Metronome.Path != "" {
		inputCount++
		args = append(args, "-i", tracks.Metronome.Path)
	}

	amerge := ""
	for i := range inputCount {
		amerge += fmt.Sprintf("[%d:a]", i)
	}
	amerge += fmt.Sprintf(" amerge=inputs=%d,pan=stereo", inputCount)

	mix := []string{}
	mergedChannelIndex := 0

	{
		for i := range track.Channels {
			mix = append(mix, fmt.Sprintf("c%d", mergedChannelIndex+i))
		}
		mergedChannelIndex += track.Channels
	}

	if tracks.Metronome.Path != "" {
		gain := flags.metronomeGain / float64(tracks.Metronome.Channels)
		for i := range tracks.Metronome.Channels {
			mix = append(mix, fmt.Sprintf("%.2f*c%d", gain, mergedChannelIndex+i))
		}
		mergedChannelIndex += tracks.Metronome.Channels
	}

	amerge += "|c0=" + strings.Join(mix, "+") + "|c1=" + strings.Join(mix, "+")
	amerge += ",loudnorm"
	args = append(args, "-filter_complex", amerge)

	dest := filepath.Join(outdir, strings.TrimSpace(filepath.Base(track.Path)))
	dest = removeExt(dest) + ".mp3"
	args = append(args, dest)

	var buffer bytes.Buffer
	fmt.Fprint(&buffer, "$ ffmpeg")
	for _, arg := range args {
		fmt.Fprintf(&buffer, " %q", arg)
	}
	fmt.Fprintln(&buffer)

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = &buffer
	cmd.Stdout = &buffer
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed %q: %w", strings.Join(cmd.Args, " "), err)
	}
	fmt.Println(buffer.String())
	return nil
}

func combinedTrack(group *errgroup.Group, outdir string, tracks Tracks, flags flags) error {
	_ = os.MkdirAll(outdir, 0755)
	// TODO:
	return nil
}

func removeExt(p string) string {
	return p[:len(p)-len(filepath.Ext(p))]
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

func readAudioChannelCount(path string) int {
	f, err := os.Open(path)
	defer func() { _ = f.Close() }()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open %q, assuming stereo\n", path)
		return 2
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".wav":
		dec := wav.NewDecoder(f)
		dec.ReadInfo()

		channels := int(dec.NumChans)
		if channels == 0 {
			fmt.Fprintf(os.Stderr, "failed to parse %q, assuming stereo\n", path)
			return 2
		}
		return int(channels)
	case ".mp3":
		return 0
	case ".aiff":
		dec := aiff.NewDecoder(f)
		dec.ReadInfo()

		channels := int(dec.NumChans)
		if channels == 0 {
			fmt.Fprintf(os.Stderr, "failed to parse %q, assuming stereo\n", path)
			return 2
		}
		return int(channels)
	case ".ogg":
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown file format %q, assuming stereo\n", path)
		return 2
	}
}
