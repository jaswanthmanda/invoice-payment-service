package config

import (
	"errors"
	"os"
)

type Config struct {
	DatabaseURL string
	PSPURL      string
	Addr        string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		PSPURL:      getenv("PSP_URL", "http://localhost:9090"),
		Addr:        getenv("API_ADDR", ":8080"),
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
