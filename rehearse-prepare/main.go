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
	globalGain    float64

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
	flag.Float64Var(&flags.globalGain, "global", 1, "global gain for full output")

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
var rxMixdown = regexp.MustCompile(`(?i)mixdown`)

// divisi parts are named e.g. "Soprano 1 (Voice 1)", "Soprano 1 (Voice_2)"
var rxVoice = regexp.MustCompile(`(?i)[ _]*\([ _]*voice[ _]*\d+[ _]*\)`)

func trackFromPath(path string) Track {
	name := removeExt(strings.TrimSpace(filepath.Base(path)))
	return Track{
		Name:     strings.TrimSpace(rxVoice.ReplaceAllString(name, "")),
		Paths:    []string{path},
		Channels: readAudioChannelCount(path),
	}
}

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
			if len(tracks.Metronome.Paths) > 0 {
				return fmt.Errorf("multiple metronome tracks, found %v and %v", tracks.Metronome, infile)
			}
			tracks.Metronome = trackFromPath(infile)
			continue
		}

		if rxMixdown.MatchString(file.Name()) {
			tracks.Mixdowns = append(tracks.Mixdowns, trackFromPath(infile))
			continue
		}

		part := trackFromPath(infile)
		if prev := tracks.Part(part.Name); prev != nil {
			prev.Paths = append(prev.Paths, infile)
			prev.Channels += part.Channels
			continue
		}
		tracks.Parts = append(tracks.Parts, part)
	}

	group := new(errgroup.Group)
	if flags.parallel > 0 {
		group.SetLimit(flags.parallel)
	}

	if len(tracks.Metronome.Paths) > 0 {
		if flags.onlyRehearsal {
			rehearsalTracks(group, filepath.Join(flags.outputDir, "Rehearse + metronome"), tracks, flags)
		}
		if flags.onlyIndividual {
			individualTracks(group, filepath.Join(flags.outputDir, "Individual + metronome"), tracks, flags)
		}
		if flags.onlyCombined {
			combinedTrack(group, flags.outputDir, tracks, flags)
		}
	}
	tracks.Metronome = Track{}

	if flags.onlyRehearsal {
		rehearsalTracks(group, filepath.Join(flags.outputDir, "Rehearse"), tracks, flags)
	}
	if flags.onlyIndividual {
		individualTracks(group, filepath.Join(flags.outputDir, "Individual"), tracks, flags)
	}
	if flags.onlyCombined {
		combinedTrack(group, flags.outputDir, tracks, flags)
	}

	for _, mixdown := range tracks.Mixdowns {
		group.Go(func() error {
			err := convertToMp3(flags.outputDir, mixdown)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Mixdown %q failed: %v\n", mixdown.Name, err)
			}
			return nil
		})
	}

	group.Wait()

	return nil
}

type Tracks struct {
	Metronome Track
	Mixdowns  []Track
	Parts     []Track
}

// Track is a single output part; it may be backed by several input files,
// when the part is split into divisi voices.
type Track struct {
	Name     string
	Paths    []string
	Channels int // total across Paths
}

// appendInputs adds "-i path" for each backing file and returns the new input count.
func (track Track) appendInputs(args []string, inputCount int) ([]string, int) {
	for _, path := range track.Paths {
		args = append(args, "-i", path)
		inputCount++
	}
	return args, inputCount
}

func (tracks *Tracks) Part(name string) *Track {
	for i := range tracks.Parts {
		if tracks.Parts[i].Name == name {
			return &tracks.Parts[i]
		}
	}
	return nil
}

func rehearsalTracks(group *errgroup.Group, outdir string, tracks Tracks, flags flags) error {
	_ = os.MkdirAll(outdir, 0755)

	rxOnly := regexp.MustCompile("(?i)" + flags.selectTracks)

	for _, track := range tracks.Parts {
		if flags.selectTracks != "" && !rxOnly.MatchString(track.Name) {
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
	var inputs []mixInput
	for _, target := range tracks.Parts {
		pan := 1 - flags.pan
		gain := flags.gain / math.Log2(float64(len(tracks.Parts)))
		if target.Name == track.Name {
			pan = 1 - pan
			gain = 1
		}
		inputs = append(inputs, mixInput{target, gain * (1 - pan), gain * pan})
	}
	if len(tracks.Metronome.Paths) > 0 {
		inputs = append(inputs, mixInput{tracks.Metronome, flags.metronomeGain, flags.metronomeGain})
	}
	return mix(filepath.Join(outdir, track.Name+".mp3"), inputs, flags)
}

type mixInput struct {
	Track       Track
	Left, Right float64
}

// mix normalizes each input on its own, pans it to stereo and sums them.
// Normalizing per input (rather than the final mix) keeps one track's gain
// from changing the loudness of the others.
func mix(dest string, inputs []mixInput, flags flags) error {
	args := []string{"-y"}
	filter := ""
	inputCount := 0
	for _, in := range inputs {
		for _, path := range in.Track.Paths {
			args = append(args, "-i", path)
			filter += fmt.Sprintf("[%d:a]loudnorm,pan=stereo|c0=%s|c1=%s[s%d];",
				inputCount, panExpr(in.Left, in.Track.Channels), panExpr(in.Right, in.Track.Channels), inputCount)
			inputCount++
		}
	}
	for i := range inputCount {
		filter += fmt.Sprintf("[s%d]", i)
	}
	filter += fmt.Sprintf("amix=inputs=%d:normalize=0:duration=shortest,aresample=48000", inputCount)
	if flags.globalGain != 1 {
		filter += fmt.Sprintf(",volume=%.2f", flags.globalGain)
	}
	args = append(args, "-filter_complex", filter, dest)

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

// panExpr averages all channels of an input into one with the given gain.
func panExpr(gain float64, channels int) string {
	terms := []string{}
	for i := range channels {
		terms = append(terms, fmt.Sprintf("%.3f*c%d", gain/float64(channels), i))
	}
	return strings.Join(terms, "+")
}

func individualTracks(group *errgroup.Group, outdir string, tracks Tracks, flags flags) error {
	_ = os.MkdirAll(outdir, 0755)

	rxOnly := regexp.MustCompile("(?i)" + flags.selectTracks)

	for _, track := range tracks.Parts {
		if flags.selectTracks != "" && !rxOnly.MatchString(track.Name) {
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
	inputs := []mixInput{{track, 1, 1}}
	if len(tracks.Metronome.Paths) > 0 {
		inputs = append(inputs, mixInput{tracks.Metronome, flags.metronomeGain, flags.metronomeGain})
	}
	return mix(filepath.Join(outdir, track.Name+".mp3"), inputs, flags)
}

func combinedTrack(group *errgroup.Group, outdir string, tracks Tracks, flags flags) error {
	_ = os.MkdirAll(outdir, 0755)
	// TODO:
	return nil
}

func convertToMp3(outdir string, track Track) error {
	_ = os.MkdirAll(outdir, 0755)

	dest := filepath.Join(outdir, track.Name+".mp3")

	args, _ := track.appendInputs([]string{"-y"}, 0)
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
		fmt.Fprintf(os.Stderr, "failed to parse %q, assuming stereo\n", path)
		return 2
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
		fmt.Fprintf(os.Stderr, "failed to parse %q, assuming stereo\n", path)
		return 2
	default:
		fmt.Fprintf(os.Stderr, "unknown file format %q, assuming stereo\n", path)
		return 2
	}
}
