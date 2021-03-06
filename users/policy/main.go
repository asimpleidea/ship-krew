package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/asimpleidea/ship-krew/users/policy/pkg/types"
	"github.com/gofiber/fiber/v2"
	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/util"
	"github.com/rs/zerolog"
)

const (
	defaultRegoDirectory string        = "/rego"
	defaultApiTimeout    time.Duration = time.Minute
	defaultPongTimeout   time.Duration = 30 * time.Second
	fiberAppName         string        = "users-policy"
)

var (
	log zerolog.Logger
)

func main() {
	var (
		verbosity     int
		regoDirectory string
	)

	flag.IntVar(&verbosity, "verbosity", 1, "the verbosity level")

	flag.StringVar(&regoDirectory, "rego-directory", defaultRegoDirectory,
		"Root directory containing rego files.")
	flag.Parse()

	log = zerolog.New(os.Stderr).With().Logger()
	log.Info().Int("verbosity", verbosity).Msg("starting...")

	{
		logLevels := [4]zerolog.Level{zerolog.DebugLevel, zerolog.InfoLevel, zerolog.ErrorLevel}
		log = log.Level(logLevels[verbosity])
	}

	ctx, canc := context.WithCancel(context.Background())
	_ = canc

	// --------------------------------------------
	// Load rego files from directory
	// --------------------------------------------

	ver, err := newVerifier(ctx, regoDirectory)
	if err != nil {
		log.Fatal().Err(err).Msg("could not load verifier")
		return
	}

	// --------------------------------------------
	// Start the gRPC server
	// --------------------------------------------

	app := fiber.New(fiber.Config{
		AppName:               fiberAppName,
		ReadTimeout:           time.Minute,
		DisableStartupMessage: verbosity > 0,
	})

	app.Post("/settings/permissions", func(c *fiber.Ctx) error {
		vctx, vcanc := context.WithTimeout(ctx, 10*time.Second)
		defer vcanc()

		perms, err := ver.verifySettingsPermissions(vctx, c.Body())
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).
				SendString(err.Error())
		}
		if err != nil {
			// TODO: better errors
			return c.Status(fiber.StatusInternalServerError).
				SendString("could not decode permissions")
		}

		return c.Status(fiber.StatusOK).JSON(perms)
	})

	internalEndpoints := fiber.New(fiber.Config{
		AppName:               fiberAppName,
		ReadTimeout:           time.Minute,
		DisableStartupMessage: verbosity > 0,
	})

	// TODO: have startup probe

	internalEndpoints.Get("/readyz", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	internalEndpoints.Get("/livez", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	go func() {
		if err := app.Listen(":8080"); err != nil {
			log.Err(err).Msg("error while listening")
		}
	}()

	go func() {
		if err := internalEndpoints.Listen(":8081"); err != nil {
			log.Err(err).Msg("error while listening")
		}
	}()

	// Graceful Shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	if err := app.Shutdown(); err != nil {
		log.Err(err).Msg("could not successfully shutdown server")
	}
	if err := internalEndpoints.Shutdown(); err != nil {
		log.Err(err).Msg("could not successfully shutdown internal server")
	}
	canc()

	log.Info().Msg("shutting down...")
	log.Info().Msg("goodbye!")
}

type verifier struct {
	settingsPermissions rego.PreparedEvalQuery
}

func newVerifier(mainCtx context.Context, regoPath string) (*verifier, error) {
	// --------------------------------------------
	// Set ups
	// --------------------------------------------

	if regoPath == "" {
		return nil, fmt.Errorf("no rego path passed")
	}

	{
		finfo, err := os.Stat(regoPath)
		if err != nil {
			return nil, fmt.Errorf(`could not load "%s": %w`, regoPath, err)
		}

		if !finfo.IsDir() {
			return nil, fmt.Errorf(`"%s" is not a directory`, regoPath)
		}
	}

	bundles := rego.LoadBundle(regoPath)

	// --------------------------------------------
	// Prepare evaluators
	// --------------------------------------------

	settingsPermissions := rego.New(rego.Query("data.users.settings.permissions"), bundles)
	spEval, err := settingsPermissions.PrepareForEval(mainCtx)
	if err != nil {
		return nil, fmt.Errorf(`could not set up "settings.permissions" evaluator: %w`, err)
	}

	return &verifier{
		settingsPermissions: spEval,
	}, nil
}

func (v *verifier) verifySettingsPermissions(ctx context.Context, data []byte) (*types.UserSettingsPermissions, error) {
	const (
		notAllowedChangeSettings string = "not_allowed_change_settings"
		cantChangeDOB            string = "cant_change_dob"
		cantChangeUsername       string = "cant_change_username"
	)

	var input interface{}
	if err := util.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("unable to parse input: %w", err)
	}

	inputValue, err := ast.InterfaceToValue(input)
	if err != nil {
		return nil, fmt.Errorf("unable to process input: %w", err)
	}

	result, err := v.settingsPermissions.Eval(ctx, rego.EvalParsedInput(inputValue))
	if err != nil {
		return nil, fmt.Errorf("cannot check permissions: %w", err)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no results set")
	}

	// TODO: can this return more than one result? or multiple expressions?
	if len(result[0].Expressions) == 0 {
		return nil, fmt.Errorf("no expressions found")
	}

	expressions := result[0].Expressions[0].Value.(map[string]interface{})
	reasons := map[string][]string{}
	for key, _val := range expressions {
		valReasons := []string{}
		val := _val.([]interface{})
		for _, r := range val {
			valReasons = append(valReasons, r.(string))
		}
		reasons[key] = valReasons
	}

	return &types.UserSettingsPermissions{
		CanModifyOwnProfile: types.Permission{
			Allowed: len(reasons[notAllowedChangeSettings]) == 0,
			Reasons: reasons[notAllowedChangeSettings],
		},
		CanChangeUsername: types.Permission{
			Allowed: len(reasons[cantChangeUsername]) == 0,
			Reasons: reasons[cantChangeUsername],
		},
		CanChangeDOB: types.Permission{
			Allowed: len(reasons[cantChangeDOB]) == 0,
			Reasons: reasons[cantChangeDOB],
		},
	}, nil
}
