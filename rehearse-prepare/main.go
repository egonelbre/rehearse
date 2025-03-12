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
	flag.Float64Var(&flags.metronomeGain, "metronome", 0.3, "metronome gain")

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
			if tracks.Metronome != "" {
				return fmt.Errorf("multiple metronome tracks, found %v and %v", tracks.Metronome, infile)
			}
			tracks.Metronome = infile
			continue
		}

		tracks.Parts = append(tracks.Parts, infile)
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
	Metronome string
	Parts     []string
}

func rehearsalTracks(group *errgroup.Group, outdir string, tracks Tracks, flags flags) error {
	_ = os.MkdirAll(outdir, 0755)

	rxOnly := regexp.MustCompile("(?i)" + flags.selectTracks)

	for target, track := range tracks.Parts {
		if flags.selectTracks != "" && !rxOnly.MatchString(track) {
			continue
		}

		group.Go(func() error {
			err := rehearsalTrack(outdir, tracks, flags, target, track)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Track %q failed: %v\n", track, err)
			}
			return nil
		})
	}

	return nil
}

func rehearsalTrack(outdir string, tracks Tracks, flags flags, target int, track string) error {
	args := []string{"-y"}

	for _, track := range tracks.Parts {
		args = append(args, "-i", track)
	}
	if tracks.Metronome != "" {
		args = append(args, "-i", tracks.Metronome)
	}

	// add inputs to -filter_complex
	amerge := ""
	for i := range tracks.Parts {
		amerge += fmt.Sprintf("[%d:a]", i)
	}
	if tracks.Metronome != "" {
		amerge += fmt.Sprintf("[%d:a]", len(tracks.Parts))
	}

	if tracks.Metronome != "" {
		amerge += fmt.Sprintf(" amerge=inputs=%d", len(tracks.Parts)+1)
	} else {
		amerge += fmt.Sprintf(" amerge=inputs=%d", len(tracks.Parts))
	}

	amerge += ",pan=stereo"
	left := ""
	right := ""
	for k := range tracks.Parts {
		pan := 1 - flags.pan
		gain := flags.gain / math.Log2(float64(len(tracks.Parts)))
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

	if tracks.Metronome != "" {
		metronome := fmt.Sprintf("+%.2f*c%d+%.2f*c%d", flags.metronomeGain, len(tracks.Parts)*2, flags.metronomeGain, len(tracks.Parts)*2+1)
		left += metronome
		right += metronome
	}

	amerge += "|c0=" + left + "|c1=" + right
	amerge += ",loudnorm"
	args = append(args, "-filter_complex", amerge)

	dest := filepath.Join(outdir, strings.TrimSpace(filepath.Base(track)))
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

	for target, track := range tracks.Parts {
		_ = target
		if flags.selectTracks != "" && !rxOnly.MatchString(track) {
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

func individualTrack(outdir string, tracks Tracks, flags flags, track string) error {
	args := []string{"-y"}

	if tracks.Metronome != "" {
		args = append(args, "-i", track, "-i", tracks.Metronome)
	} else {
		args = append(args, "-i", track)
	}

	amerge := ""
	if tracks.Metronome != "" {
		amerge = "[0:a][1:a] amerge=inputs=2"
	} else {
		amerge = "[0:a] amerge=inputs=1"
	}
	amerge += ",pan=stereo"

	var mix string
	if tracks.Metronome != "" {
		mix = fmt.Sprintf("c0+c1+%.2f*c2+%.2f*c3", flags.metronomeGain, flags.metronomeGain)
	} else {
		mix = "c0+c1"
	}

	amerge += "|c0=" + mix + "|c1=" + mix
	amerge += ",loudnorm"
	args = append(args, "-filter_complex", amerge)

	dest := filepath.Join(outdir, strings.TrimSpace(filepath.Base(track)))
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
