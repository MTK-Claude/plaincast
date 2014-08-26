package mp

// This library is a wrapper around a media player that plays YouTube playlists.

import (
	"fmt"
	"os"
	"time"
)

// these are defined by the YouTube API
type State int

const (
	STATE_STOPPED State = iota
	STATE_PLAYING
	STATE_PAUSED
	STATE_BUFFERING
)

type PlayState struct {
	Playlist          []string
	Index             int
	State             State
	Volume            int
	bufferingPosition time.Duration
}

type StateChange struct {
	State    State
	Position time.Duration
}

const INITIAL_VOLUME = 80

func log(v ...interface{}) {
	os.Stdout.WriteString(time.Now().Format("15:04:05.000 ") + fmt.Sprintln(v...))
}
