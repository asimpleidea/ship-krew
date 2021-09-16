package main

import (
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	flag "github.com/spf13/pflag"
)

var (
	log zerolog.Logger
)

func main() {
	usersApiAddress := "users-api-server.ship-krew-backend:8081"
	testSpaces := 0
	verbosity := 1

	flag.StringVar(&usersApiAddress, "users-api-server", usersApiAddress, "the address where to contact the users API server")
	flag.IntVar(&testSpaces, "test-spaces", 0, "the number of test spaces to create")
	flag.IntVarP(&verbosity, "verbosity", "v", 1, "the verbosity level")
	flag.Parse()

	log = zerolog.New(os.Stderr).With().Timestamp().Logger()

	verbosityLevels := []zerolog.Level{zerolog.DebugLevel, zerolog.InfoLevel, zerolog.ErrorLevel}
	if verbosity < 0 || verbosity > len(verbosityLevels) {
		log.Error().
			Int("verbosity", verbosity).
			Int("default", 1).
			Msg("invalid verbosity level provided, reverting to default...")
		verbosity = 1
	}

	if testSpaces > 0 {
		log.Info().
			Int("test-spaces", testSpaces).
			Msg("test spaces requested")
		verbosity = 0
	}

	log = log.Level(verbosityLevels[verbosity]).With().Logger()
	log.Info().Msg("starting...")

	// Probes
	probes := fiber.New()
	probes.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendStatus(200)
	})
	probes.Get("/ready", func(c *fiber.Ctx) error {
		resp, err := http.Get(fmt.Sprintf("%s/ready", usersApiAddress))
		if err != nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		if resp.StatusCode != fiber.StatusOK {
			return c.SendStatus(fiber.StatusInternalServerError)
		}

		return c.SendStatus(fiber.StatusOK)
	})

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		probes.Listen(":8081")
	}()

	wg.Wait()
}
