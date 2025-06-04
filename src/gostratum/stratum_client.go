package gostratum

import (
	"bufio"
	"io"
	"net"
	"os"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/zap"
)

func spawnClientListener(ctx *StratumContext, connection net.Conn, s *StratumListener) error {
	defer ctx.Disconnect()

	for {
		err := readFromConnection(connection, func(line string) error {
			// ctx.Logger.Info("client message: ", zap.String("raw", line))
			event, err := UnmarshalEvent(line)
			if err != nil {
				ctx.Logger.Error("error unmarshalling event", zap.String("raw", line))
				return err
			}
			return s.HandleEvent(ctx, event)
		}, ctx)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue // expected timeout
		}
		if ctx.Err() != nil {
			return ctx.Err() // context cancelled
		}
		if ctx.parentContext.Err() != nil {
			return ctx.parentContext.Err() // parent context cancelled
		}
		if err != nil { // actual error
			ctx.Logger.Error("error reading from socket", zap.Error(err))
			return err
		}
	}
}

type LineCallback func(line string) error

func readFromConnection(connection net.Conn, cb LineCallback, ctx *StratumContext) error {
	deadline := time.Now().Add(5 * time.Second).UTC()
	if err := connection.SetReadDeadline(deadline); err != nil {
		return err
	}

	scanner := bufio.NewScanner(connection)
	for scanner.Scan() {
		if err := cb(scanner.Text()); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if err == io.EOF {
			ctx.Logger.Info("client disconnected (EOF)")
			return nil
		}
		return errors.Wrapf(err, "error reading from connection")
	}
	return nil
}
