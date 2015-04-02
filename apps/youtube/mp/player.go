package mp

import (
	"log"
	"time"
)

// A generic YouTube media player using a playlist.
type MediaPlayer struct {
	player      Backend
	stateChange chan StateChange

	// A channel to coordinate access to the PlayState.
	// The pointer to the PlayState is used as an access token.
	playstateChan chan PlayState

	vg *VideoGrabber
}

func New(stateChange chan StateChange) *MediaPlayer {
	p := MediaPlayer{}
	p.stateChange = stateChange
	p.playstateChan = make(chan PlayState)
	p.vg = NewVideoGrabber()

	p.player = &MPV{}
	playerEventChan, initialVolume := p.player.initialize()

	// Start the mainloop.
	go p.run(playerEventChan, initialVolume)

	return &p
}

// Quit quits the MediaPlayer.
// No other method may be called upon this object after this function has been
// called.
func (p *MediaPlayer) Quit() {
	p.getPlayState(func(ps *PlayState) {
		p.player.quit()
		p.vg.Quit()
	})
}

func (p *MediaPlayer) getPosition(ps *PlayState) time.Duration {
	var position time.Duration

	switch ps.State {
	case STATE_STOPPED:
		position = 0
	case STATE_BUFFERING, STATE_SEEKING:
		position = ps.bufferingPosition
	case STATE_PLAYING, STATE_PAUSED:
		var err error
		position, err = p.player.getPosition()
		if err != nil {
			// TODO: the position might be unavailable just after a seek
			panic(err)
		}
	default:
		panic("unknown state")
	}

	if position < 0 {
		panic("got position < 0")
	}

	return position
}

// getPlayState gets the play state for use in a callback.
// The *PlayState argument may only be used until the callback exits to prevent
// race conditions.
func (p *MediaPlayer) getPlayState(callback func(*PlayState)) {
	ps, ok := <-p.playstateChan
	if !ok {
		// The player has already stopped. Ignore all function calls.
		return
	}
	callback(&ps)
	p.playstateChan <- ps
}

// SetPlaystate changes the play state to the specified arguments
// This function doesn't block, but changes may not be immediately applied.
func (p *MediaPlayer) SetPlaystate(playlist []string, index int, position time.Duration) {
	p.getPlayState(func(ps *PlayState) {
		if ps.State == STATE_BUFFERING && ps.bufferingPosition == position && ps.Index < len(ps.Playlist) && playlist[index] == ps.Playlist[ps.Index] {
			// just in case something else has changed, update the playlist
			p.updatePlaylist(ps, playlist)
			return
		}
		ps.Playlist = playlist
		ps.Index = index

		if len(ps.Playlist) > 0 {
			p.startPlaying(ps, position)
		} else {
			p.stop(ps)
		}
	})
}

func (p *MediaPlayer) startPlaying(ps *PlayState, position time.Duration) {
	if ps.State == STATE_PLAYING {
		// Pause the currently playing track.
		// This has multiple benefits:
		//  *  When pressing 'play', the user probably expects the next video to
		//     be played immediately, or if that is not possible, expects the
		//     current video to stop playing.
		//  *  On very slow systems, like the Raspberry Pi, downloading the
		//     stream URL for the next video doesn't interrupt the currently
		//     playing video.
		p.player.stop()
	}
	p.setPlayState(ps, STATE_BUFFERING, position)

	videoId := ps.Playlist[ps.Index]

	go func() {
		// Do not use the playstate inside the goroutine to prevent race conditions.
		// A new goroutine loses rights to the PlayState structure, enforce that
		// rule here.
		ps = nil

		streamUrl := p.vg.GetStream(videoId)

		// again acquire PlayState access
		p.getPlayState(func(ps *PlayState) {
			// Check whether another video has been queued to be played already:
			// one may be played while the URL for another is still being
			// fetched.
			if ps.Video() != videoId {
				log.Printf("Video %s isn't needed anymore\n", videoId)
				return
			}

			if streamUrl == "" {
				// Failed to get a stream.
				// Try to play the next.
				log.Println("WARNING: empty stream URL")
				p.nextVideo(ps)
				return
			}

			volume := -1
			if ps.newVolume {
				ps.newVolume = false
				volume = ps.Volume
			}

			p.player.play(streamUrl, position, volume)

			go p.prefetchVideoStream(ps.NextVideo())
		})
	}()
}

func (p *MediaPlayer) nextVideo(ps *PlayState) {
	if ps.Index+1 < len(ps.Playlist) {
		// there are more videos, play the next
		ps.Index++
		// p.startPlaying sets the playstate immediately to
		// buffering (using setPlayState), so it's okay to change it
		// here. And it is needed, otherwise startPlaying will pause
		// the currently 'playing' track causing an error in MPV
		// (nothing is playing, so nothing can be paused).
		ps.State = STATE_STOPPED
		p.startPlaying(ps, 0)
	} else {
		// signal that the video has stopped playing
		// this resets the position but keeps the playlist
		p.setPlayState(ps, STATE_STOPPED, 0)
	}
}

// Prefetch the next video after the current video has played for a
// short while.
//
// Warning: start this function in a new goroutine!
func (p *MediaPlayer) prefetchVideoStream(videoId string) {
	if videoId == "" {
		return
	}

	time.Sleep(10 * time.Second)

	p.getPlayState(func(ps *PlayState) {
		next := ps.NextVideo()

		if next == "" || next != videoId {
			// The playlist has changed in the meantime
			return
		}

		go p.vg.GetStream(next)
	})
}

// setPlayState updates the PlayState and sends events.
// position may be -1: in that case it will be updated.
func (p *MediaPlayer) setPlayState(ps *PlayState, state State, position time.Duration) {
	if ps.State == STATE_SEEKING {
		position = ps.bufferingPosition
	}

	ps.previousState = ps.State
	ps.State = state

	if state == STATE_BUFFERING || state == STATE_SEEKING {
		ps.bufferingPosition = position
	} else {
		ps.bufferingPosition = -1
	}

	if position == -1 {
		position = p.getPosition(ps)
	}

	p.stateChange <- StateChange{state, position}
}

func (p *MediaPlayer) UpdatePlaylist(playlist []string) {
	p.getPlayState(func(ps *PlayState) {
		p.updatePlaylist(ps, playlist)
	})
}

func (p *MediaPlayer) updatePlaylist(ps *PlayState, playlist []string) {
	nextVideo := ps.NextVideo()

	if len(ps.Playlist) == 0 {

		if ps.State == STATE_PLAYING {
			// just to be sure
			panic("empty playlist while playing")
		}
		ps.Playlist = playlist

		if ps.Index >= len(playlist) {
			// this appears to be the normal behavior of YouTube
			ps.Index = len(playlist) - 1
		}

	} else {
		videoId := ps.Playlist[ps.Index]
		ps.Playlist = playlist
		p.setPlaylistIndex(ps, videoId)
	}

	if ps.NextVideo() != nextVideo {
		go p.prefetchVideoStream(ps.NextVideo())
	}
}

func (p *MediaPlayer) SetVideo(videoId string, position time.Duration) {
	p.getPlayState(func(ps *PlayState) {
		p.setPlaylistIndex(ps, videoId)
		p.startPlaying(ps, position)
	})
}

func (p *MediaPlayer) setPlaylistIndex(ps *PlayState, videoId string) {
	newIndex := -1
	for i, v := range ps.Playlist {
		if v == videoId {
			if newIndex >= 0 {
				log.Println("WARNING: videoId exists twice in playlist")
				break
			}
			newIndex = i
			// no 'break' so duplicate video entries can be checked
		}
	}

	if newIndex == -1 {
		// don't know how to proceed
		// TODO: it is currently possible an invalid message could cause this
		// error (via updatePlaylist or setVideo).
		panic("current video does not exist in new playlist")
	}

	ps.Index = newIndex
}

// RequestPlaylist asynchronously gets the playlist state and sends it over the
// channel.
// To make asynchronous requests work, it expects a 1-buffered channel. Before a
// new PlaylistState is sent over the channel, the previous is read if it's
// there. It ensures that only one goroutine does that at one time, so this
// trick should not be used elsewhere on the same channel.
func (p *MediaPlayer) RequestPlaylist(playlistChan chan PlaylistState) {
	go p.getPlayState(func(ps *PlayState) {
		playlist := make([]string, len(ps.Playlist))
		copy(playlist, ps.Playlist)

		// If there is a value in the (buffered) channel, clear it.
		// Only one goroutine at a time can do this, because they're guarded by
		// getPlayState. This makes sure the request can run in a goroutine
		// while no goroutines are being leaked and values always arrive in
		// order.
		select {
		case <-playlistChan:
		default:
		}
		playlistChan <- PlaylistState{playlist, ps.Index, p.getPosition(ps), ps.State}
	})
}

// Pause pauses the currently playing video
func (p *MediaPlayer) Pause() {
	p.getPlayState(func(ps *PlayState) {
		if ps.State == STATE_SEEKING {
			ps.nextState = STATE_PAUSED
		} else if ps.State != STATE_PLAYING {
			log.Printf("Warning: pause while in state %d - ignoring\n", ps.State)
		} else {
			p.player.pause()
		}
	})
}

// Play resumes playback when it was paused
func (p *MediaPlayer) Play() {
	p.getPlayState(func(ps *PlayState) {
		if ps.State == STATE_STOPPED {
			// Restart from the beginning.
			if ps.Index >= len(ps.Playlist) {
				log.Println("Warning: invalid index or empty playlist")
				return
			}
			p.startPlaying(ps, 0)

		} else if ps.State == STATE_SEEKING {
			ps.nextState = STATE_PLAYING

		} else {
			if ps.State != STATE_PAUSED {
				log.Printf("Warning: resume while in state %d - ignoring\n", ps.State)
			} else {
				p.player.resume()
			}
		}
	})
}

// Seek jumps to the specified position
func (p *MediaPlayer) Seek(position time.Duration) {
	p.getPlayState(func(ps *PlayState) {
		if ps.State == STATE_STOPPED {
			p.startPlaying(ps, position)
		} else if ps.State == STATE_PAUSED || ps.State == STATE_PLAYING {
			p.setPlayState(ps, STATE_SEEKING, position)
			p.player.setPosition(position)
		} else {
			log.Printf("Warning: state is not paused or playing while seeking (state: %d) - ignoring\n", ps.State)
		}
	})
}

// SetVolume sets the volume of the player to the specified value (0-100).
func (p *MediaPlayer) SetVolume(volume int, volumeChan chan int) {
	p.getPlayState(func(ps *PlayState) {
		ps.Volume = volume
		p.applyVolume(ps, volumeChan)
	})
}

// ChangeVolume increases or decreases the volume by the specified delta.
func (p *MediaPlayer) ChangeVolume(delta int, volumeChan chan int) {
	p.getPlayState(func(ps *PlayState) {
		ps.Volume += delta
		// pressing 'volume up' or 'volume down' keeps sending volume
		// increase/decrease messages. Keep the volume within range 0-100.
		if ps.Volume < 0 {
			ps.Volume = 0
		}
		if ps.Volume > 100 {
			ps.Volume = 100
		}

		p.applyVolume(ps, volumeChan)
	})
}

func (p *MediaPlayer) applyVolume(ps *PlayState, volumeChan chan int) {
	if ps.State == STATE_PLAYING || ps.State == STATE_PAUSED {
		p.player.setVolume(ps.Volume)
	} else {
		ps.newVolume = true
	}
	volumeChan <- ps.Volume
}

// RequestVolume asynchronously gets the volume and sends it over the channel
// volumeChan. See RequestPlaylist for how this works.
func (p *MediaPlayer) RequestVolume(volumeChan chan int) {
	go p.getPlayState(func(ps *PlayState) {

		select {
		case <-volumeChan:
		default:
		}
		volumeChan <- ps.Volume
	})
}

func (p *MediaPlayer) stop(ps *PlayState) {
	ps.Playlist = []string{}
	// Do not set ps.Index to 0, it may be needed for UpdatePlaylist:
	// Stop is called before UpdatePlaylist when removing the currently
	// playing video from the playlist.
	p.player.stop()
}

// Stop stops the currently playing sound and clears the playlist.
func (p *MediaPlayer) Stop() {
	p.getPlayState(p.stop)
}

// Function run is the mainloop of the player. It mainly handles state change
// events.
func (p *MediaPlayer) run(playerEventChan chan State, initialVolume int) {
	ps := PlayState{}
	ps.Volume = initialVolume
	ps.nextState = -1

	for {
		select {
		case p.playstateChan <- ps:
			// Synchronize access to the PlayState structure.
			// See the documentation of PlayState.
			ps = <-p.playstateChan

		case event, ok := <-playerEventChan:
			if !ok {
				// player has quit, and closed channel
				close(p.stateChange)
				close(p.playstateChan)
				return
			}

			switch event {
			case STATE_PLAYING:
				if ps.newVolume {
					ps.newVolume = false
					p.player.setVolume(ps.Volume)
				}

				if ps.State == STATE_SEEKING {
					if ps.nextState != -1 && ps.previousState != ps.nextState {
						ps.State = ps.previousState

						state := ps.nextState
						ps.nextState = -1

						switch state {
						case STATE_PLAYING:
							p.player.resume()
						case STATE_PAUSED:
							p.player.pause()
						default:
							panic("unknown nextState")
						}
					} else {
						p.setPlayState(&ps, ps.previousState, -1)
					}
					break
				}

				p.setPlayState(&ps, STATE_PLAYING, -1)

			case STATE_PAUSED:
				if ps.State == STATE_BUFFERING {
					// The video has been paused while the stream for the next
					// video is being loaded.
					break
				}

				p.setPlayState(&ps, STATE_PAUSED, -1)

			case STATE_STOPPED:
				if ps.State == STATE_BUFFERING {
					// The previous video has stopped on a 'loadfile' command in
					// MPV. This is expected and should be ignored.
					break
				}

				// There may be more videos.
				p.nextVideo(&ps)
			}
		}
	}
}
