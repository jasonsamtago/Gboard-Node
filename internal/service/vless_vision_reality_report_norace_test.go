//go:build !race

package service

import "testing"

func skipVisionSpliceUnderRace(*testing.T) {}
