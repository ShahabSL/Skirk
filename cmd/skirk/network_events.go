package main

import (
	"bufio"
	"context"
	"io"
	"log"
)

func watchClientNetworkEvents(ctx context.Context, input io.Reader, refresh func()) {
	reader := bufio.NewReader(input)
	for ctx.Err() == nil {
		event, err := reader.ReadByte()
		if err != nil {
			return
		}
		if event == 'N' && ctx.Err() == nil {
			refresh()
			log.Print("default network changed; refreshed idle carrier connections")
		}
	}
}
