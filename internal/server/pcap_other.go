//go:build !linux

package server

import (
	"context"
	"errors"
)

func capture(context.Context, string, pcapFilter, int64, func([]byte) error) error {
	return errors.New("packet capture needs Linux")
}
