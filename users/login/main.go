package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/asimpleidea/ship-krew/users/api/pkg/api"
	uerrors "github.com/asimpleidea/ship-krew/users/api/pkg/errors"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/encryptcookie"
	"github.com/gofiber/template/html"
	"github.com/rs/zerolog"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
	"gopkg.in/yaml.v3"
)

// TODO: Follow https://stackoverflow.com/questions/244882/what-is-the-best-way-to-implement-remember-me-for-a-website
// TODO: follow https://paragonie.com/blog/2015/04/secure-authentication-php-with-long-term-persistence#title.2

const (
	fiberAppName      string        = "Login Backend"
	defaultApiTimeout time.Duration = time.Minute
)

var (
	log zerolog.Logger
)

func main() {
	var (
		verbosity     int
		usersApiAddr  string
		timeout       time.Duration
		cookieKey     string
		etcdEndpoints string
	)

	flag.IntVar(&verbosity, "verbosity", 1, "the verbosity level")

	// TODO: https, not http
	flag.StringVar(&usersApiAddr, "users-api-address", "http://users-api", "the address of the users server API")
	flag.DurationVar(&timeout, "timeout", 2*time.Minute, "requests timeout")

	// TODO: this should be pulled from secrets
	flag.StringVar(&cookieKey, "cookie-key", "", "The key to un-encrypt cookies")

	// TODO:
	// - this must be slices
	// - use default
	// - use secrets for authentication
	flag.StringVar(&etcdEndpoints, "etcd-endpoints", "http://localhost:2379",
		"Endpoints where to contact etcd.")
	flag.Parse()

	log = zerolog.New(os.Stderr).With().Logger()
	log.Info().Int("verbosity", verbosity).Msg("starting...")

	{
		logLevels := [4]zerolog.Level{zerolog.DebugLevel, zerolog.InfoLevel, zerolog.ErrorLevel}
		log = log.Level(logLevels[verbosity])
	}

	if cookieKey == "" {
		log.Fatal().Err(errors.New("no cookie key set")).Msg("fatal error occurred")
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"localhost:2379"},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatal().Err(err).Msg("cloud not connect to etcd")
	}
	sessionsKV := namespace.NewKV(cli.KV, "sessions/")
	defer cli.Close()

	// TODO: if not available should fail
	engine := html.New("./views", ".html")

	// TODO: authenticate to users server with APIKey

	app := fiber.New(fiber.Config{
		AppName:               fiberAppName,
		ReadTimeout:           time.Minute,
		DisableStartupMessage: verbosity > 0,
		Views:                 engine,
	})

	app.Use(encryptcookie.New(encryptcookie.Config{
		Key: cookieKey,
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		// TODO: make this whole function better
		ctx, canc := context.WithTimeout(context.Background(), defaultApiTimeout)
		usrSession, sessionID, _ := getSessionFromEtcd(ctx, c, sessionsKV)
		// TODO: check if error is from etcd, if so internal server error
		// otherwise just login
		canc()
		if usrSession != nil {
			if !usrSession.Expired() {
				if usrSession.DaysTillExpiration() < 3 {
					crCtx, crCanc := context.WithTimeout(context.Background(), defaultApiTimeout)
					usrSession.Expiration = time.Now().AddDate(0, 0, 7)
					err := createSessionOnEtcd(crCtx, sessionsKV, *sessionID, usrSession)
					crCanc()
					if err != nil {
						log.Err(err).Str("session-id", *sessionID).
							Int64("user-id", usrSession.UserID).
							Msg("error while trying to update session")
					}

				}

				return c.Status(fiber.StatusNotFound).SendString("already logged in")
			}

			func() {
				delCtx, delCanc := context.WithTimeout(context.Background(), defaultApiTimeout)
				defer delCanc()
				if err := deleteSession(delCtx, c, sessionsKV, *sessionID); err != nil {
					log.Err(err).Str("session-id", *sessionID).
						Int64("user-id", usrSession.UserID).
						Msg("error while trying to delete session")
				}
			}()
		}

		// TODO:
		// - check if session exists on etcd
		// - check if it corresponds to this user
		// - check if not expired

		// TODO:
		// - This must be called login
		return c.Render("index", fiber.Map{
			"Title": "Login",
		})
	})

	app.Post("/", func(c *fiber.Ctx) error {
		ctx, canc := context.WithTimeout(context.Background(), defaultApiTimeout)
		usrSession, sessionID, _ := getSessionFromEtcd(ctx, c, sessionsKV)
		// TODO: check if error is from etcd, if so internal server error
		// otherwise just login
		canc()
		if usrSession != nil {
			if !usrSession.Expired() {
				if usrSession.DaysTillExpiration() < 3 {
					crCtx, crCanc := context.WithTimeout(context.Background(), defaultApiTimeout)
					usrSession.Expiration = time.Now().AddDate(0, 0, 7)
					err := createSessionOnEtcd(crCtx, sessionsKV, *sessionID, usrSession)
					crCanc()
					if err != nil {
						log.Err(err).Str("session-id", *sessionID).
							Int64("user-id", usrSession.UserID).
							Msg("error while trying to update session")
					}
				}

				return c.Status(fiber.StatusNotFound).SendString("already logged in")
			}

			func() {
				delCtx, delCanc := context.WithTimeout(context.Background(), defaultApiTimeout)
				defer delCanc()
				if err := deleteSession(delCtx, c, sessionsKV, *sessionID); err != nil {
					log.Err(err).Str("session-id", *sessionID).
						Int64("user-id", usrSession.UserID).
						Msg("error while trying to delete session")
				}
			}()
		}

		// TODO:
		// - validations
		// - check if values are actually provided
		// - check if ajax
		const (
			formUsername = "login_username"
			formPassword = "login_password"
		)

		username := c.FormValue(formUsername)
		pwd := c.FormValue(formPassword)

		ctx, canc = context.WithTimeout(context.Background(), defaultApiTimeout)
		usr, err := getUserByUsername(ctx, usersApiAddr, username)
		if err != nil {
			canc()
			// TODO:
			// - do not disclose that this user does not exist, but just say
			// 	that this user-pwd combination was not found
			var e *uerrors.Error
			if errors.As(err, &e) {
				return c.Status(uerrors.ToHTTPStatusCode(e.Code)).
					JSON(e)
			}

			return c.Status(fiber.StatusBadRequest).SendString("not ok")
		}
		canc()

		if passwordIsCorrect(pwd, usr.Base64PasswordHash, usr.Base64Salt) {
			// TODO: generate a good session ID
			sessionID := "testing"
			c.Cookie(&fiber.Cookie{
				Name:  "session",
				Value: sessionID,
			})

			ctx, canc = context.WithTimeout(context.Background(), defaultApiTimeout)
			defer canc()
			if err := createSessionOnEtcd(ctx, sessionsKV, sessionID, &UserSession{
				CreatedAt:  time.Now(),
				UserID:     usr.ID,
				Expiration: time.Now().AddDate(0, 0, 7),
			}); err != nil {
				return c.Status(fiber.StatusInternalServerError).
					Send([]byte(err.Error()))
			}

			return c.Status(fiber.StatusOK).Send([]byte("ok"))
		}

		// TODO: cookie

		return c.Status(fiber.StatusOK).
			Send([]byte("does not match"))
	})

	app.Get("/logout", func(c *fiber.Ctx) error {
		ctx, canc := context.WithTimeout(context.Background(), defaultApiTimeout)
		usrSession, sessID, _ := getSessionFromEtcd(ctx, c, sessionsKV)
		canc()

		if usrSession == nil {
			return c.Status(fiber.StatusNotFound).
				SendString("not logged int")
		}

		// TODO:
		// - check if session exists on etcd
		// - check if it corresponds to this user
		// - check if not expired

		c.ClearCookie("session")
		ctx, canc = context.WithTimeout(context.Background(), defaultApiTimeout)
		if _, err := sessionsKV.Delete(ctx, *sessID); err != nil {
			log.Err(err).Str("session-id", *sessID).
				Int64("user-id", usrSession.UserID).
				Msg("error while trying to delete session")
			canc()
		}
		canc()

		// TODO: redirect
		return c.Status(fiber.StatusOK).SendString("ok")
	})

	go func() {
		if err := app.Listen(":8080"); err != nil {
			log.Err(err).Msg("error while listening")
		}
	}()

	// Graceful Shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop

	log.Info().Msg("shutting down...")
	if err := app.Shutdown(); err != nil {
		log.Err(err).Msg("error while waiting for server to shutdown")
	}
	log.Info().Msg("goodbye!")
}

// TODO: this must be integrated in the client
func getUserByUsername(ctx context.Context, usersApiAddr, username string) (*api.User, error) {
	req, err := http.NewRequestWithContext(ctx,
		http.MethodGet,
		fmt.Sprintf("%s/users/username/%s", usersApiAddr, username),
		nil)
	if err != nil {
		return nil, err
	}

	// TODO: use cookies in client?
	cl := &http.Client{}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	// TODO: better way to handle these internal server error

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, &uerrors.Error{
			Code:    uerrors.CodeInternalServerError,
			Message: uerrors.MessageInternalServerError,
		}
	}

	if resp.StatusCode != fiber.StatusOK {
		var e uerrors.Error
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, &uerrors.Error{
				Code:    uerrors.CodeInternalServerError,
				Message: uerrors.MessageInternalServerError,
			}
		}

		return nil, &e
	}

	var user api.User
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, &uerrors.Error{
			Code:    uerrors.CodeInternalServerError,
			Message: uerrors.MessageInternalServerError,
		}
	}

	return &user, nil
}

// TODO: this may need to be better and maybe done on client
func passwordIsCorrect(provided string, expected, salt *string) bool {
	decodedExpected, _ := base64.URLEncoding.DecodeString(*expected)
	decodedSalt, _ := base64.URLEncoding.DecodeString(*salt)

	digestProvided := sha256.Sum256([]byte(provided))
	passWithSalt := append(digestProvided[:], decodedSalt...)

	return bytes.Equal(passWithSalt, decodedExpected)
}

type UserSession struct {
	CreatedAt  time.Time `json:"created_at" yaml:"createdAt"`
	UserID     int64     `json:"user_id" yaml:"userId"`
	Expiration time.Time `json:"expiration" yaml:"expiration"`
}

func (u *UserSession) Expired() bool {
	return time.Now().After(u.Expiration)
}

func (u *UserSession) DaysTillExpiration() int {
	if u.Expired() {
		return -1
	}

	return int(time.Until(u.Expiration).Hours() / 24)
}

func getSessionFromEtcd(ctx context.Context, fctx *fiber.Ctx, sessionsKV clientv3.KV) (*UserSession, *string, error) {
	// TODO: not sure about bringing fctx here
	sessionID := fctx.Cookies("session", "")
	if sessionID == "" {
		return nil, nil, nil
	}

	storedSess, err := sessionsKV.Get(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}

	if storedSess.Count == 0 {
		return nil, nil, nil
	}

	if len(storedSess.Kvs) == 0 {
		// TODO: better error
		return nil, nil, fmt.Errorf("no keys found")
	}

	val := storedSess.Kvs[0].Value
	var usrSession UserSession
	if err := yaml.Unmarshal(val, &usrSession); err != nil {
		return nil, nil, err
	}

	return &usrSession, &sessionID, nil
}

func createSessionOnEtcd(ctx context.Context, sessionKV clientv3.KV, sessionID string, usrSession *UserSession) error {
	val, err := yaml.Marshal(usrSession)
	if err != nil {
		return err
	}

	_, err = sessionKV.Put(ctx, sessionID, string(val))
	if err != nil {
		return err
	}

	return nil
}

func deleteSession(ctx context.Context, fctx *fiber.Ctx, sessionsKV clientv3.KV, sessionID string) error {
	fctx.ClearCookie("session")

	_, err := sessionsKV.Delete(ctx, sessionID)
	if err != nil {
		return err
	}

	return nil
}
