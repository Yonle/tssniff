package main

import (
	"os"
	"strings"
)

type FAT32Filesystem struct{}

func (FAT32Filesystem) Name() string {
	return "FAT32"
}

func (FAT32Filesystem) Probe(
	fd *os.File,
	partition Partition,
) (bool, error) {
	_, err := readFAT32Layout(fd, partition)

	if err != nil {
		if strings.Contains(err.Error(), "invalid FAT32") ||
			strings.Contains(err.Error(), "FAT16 FAT size field") ||
			strings.Contains(err.Error(), "zero FAT32") {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func (FAT32Filesystem) NewTracker(
	fd *os.File,
	partition Partition,
) (Tracker, error) {
	return NewFAT32Tracker(fd, partition)
}
