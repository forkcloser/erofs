//go:build race

package erofs_test

// raceDetector says the tests were built with the race detector, under
// which a sync.Pool drops a share of what it is given back.
const raceDetector = true
