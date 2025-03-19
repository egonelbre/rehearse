package main

import (
	"encoding/xml"
	"fmt"
	"os"

	"github.com/egonelbre/rehearse/internal/musicxml"
	"github.com/kr/pretty"
)

var _ = pretty.Println

func main() {
	data := must(os.ReadFile("testdata/Põhjatuul ja päike.musicxml"))

	var score musicxml.ScorePartwise
	must0(xml.Unmarshal(data, &score))

	parts := map[string]*musicxml.ScorePart{}
	for _, part := range score.PartList.GroupScorePart.ScorePart {
		parts[part.Id] = part
	}

	for _, part := range score.Part {
		info := parts[part.Id]
		fmt.Println("P", info.PartName.EnclosedText)
		for _, measure := range part.Measure {
			fmt.Println("M", measure.Number)

			divisions := 4
			_ = divisions
			for _, el := range measure.Element {
				switch value := el.Value.(type) {
				case *musicxml.Print: // ignore
				case *musicxml.Attributes:
					if value.Divisions != 0 {
						divisions = value.Divisions
					}
					for _, time := range value.Time {
						fmt.Println("TIME CHANGE", time.Beats, time.BeatType)
					}
				default:
					fmt.Fprintf(os.Stderr, "unhandled type %T\n", value)
				}
			}
		}
	}

	fmt.Printf("%#v\n", score)
}

func must0(err error) {
	if err != nil {
		panic(err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
