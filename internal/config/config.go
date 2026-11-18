package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

const defaultAddr = "127.0.0.1:8080"

type Config struct {
	Addr string
}

func Load() (Config, error) {
	addr, set := os.LookupEnv("FILEDOCK_ADDR")
	if !set {
		addr = defaultAddr
	}

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return Config{}, fmt.Errorf("invalid FILEDOCK_ADDR: %w", err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return Config{}, fmt.Errorf("invalid FILEDOCK_ADDR %q: port must be a number from 0 to 65535", addr)
	}
	return Config{Addr: addr}, nil
}
