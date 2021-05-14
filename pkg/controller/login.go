package controller

import (
	"fmt"
	"github.com/form3tech-oss/jwt-go"
	"github.com/getsentry/sentry-go"
	"github.com/go-pg/pg/v10"
	"github.com/kataras/iris/v12"
	"github.com/monetrapp/rest-api/pkg/hash"
	"github.com/monetrapp/rest-api/pkg/models"
	"github.com/monetrapp/rest-api/pkg/repository"
	"github.com/pkg/errors"
	"github.com/stripe/stripe-go/v72"
	"net/http"
	"strings"
	"time"
)

type HarderClaims struct {
	LoginId            uint64 `json:"loginId"`
	UserId             uint64 `json:"userId"`
	AccountId          uint64 `json:"accountId"`
	SubscriptionStatus bool   `json:"subStatus"`
	jwt.StandardClaims
}

// Login
// @Summary Login
// @id login
// @tags Authentication
// @description Authenticate a user.
// @Accept json
// @Produce json
// @Param Login body swag.LoginRequest true "User Login Request"
// @Router /authentication/login [post]
// @Success 200 {object} swag.LoginResponse
// @Failure 400 {object} ApiError Required data is missing.
// @Failure 403 {object} ApiError Invalid credentials.
// @Failure 500 {object} ApiError Something went wrong on our end.
func (c *Controller) loginEndpoint(ctx iris.Context) {
	var loginRequest struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Captcha  string `json:"captcha"`
	}
	if err := ctx.ReadJSON(&loginRequest); err != nil {
		c.wrapAndReturnError(ctx, err, http.StatusBadRequest, "failed to decode login request")
		return
	}

	// This will take the captcha from the request and validate it if the API is
	// configured to do so. If it is enabled and the captcha fails then an error
	// is returned to the client.
	if err := c.validateCaptchaMaybe(c.getContext(ctx), loginRequest.Captcha); err != nil {
		c.wrapAndReturnError(ctx, err, http.StatusBadRequest, "valid ReCAPTCHA is required")
		return
	}

	loginRequest.Email = strings.ToLower(strings.TrimSpace(loginRequest.Email))
	loginRequest.Password = strings.TrimSpace(loginRequest.Password)

	if err := c.validateLogin(loginRequest.Email, loginRequest.Password); err != nil {
		c.wrapAndReturnError(ctx, err, http.StatusBadRequest, "login is not valid")
		return
	}

	hashedPassword := hash.HashPassword(loginRequest.Email, loginRequest.Password)
	var login models.Login
	if err := c.db.RunInTransaction(c.getContext(ctx), func(txn *pg.Tx) error {
		return txn.ModelContext(c.getContext(ctx), &login).
			Relation("Users").
			Relation("Users.Account").
			Where(`"login"."email" = ? AND "login"."password_hash" = ?`, loginRequest.Email, hashedPassword).
			Limit(1).
			Select(&login)
	}); err != nil {
		if err == pg.ErrNoRows {
			c.returnError(ctx, http.StatusForbidden, "invalid email and password")
			return
		}

		c.wrapAndReturnError(ctx, err, http.StatusInternalServerError, "failed to authenticate")
		return
	}

	switch len(login.Users) {
	case 0:
		// TODO (elliotcourant) Should we allow them to create an account?
		c.returnError(ctx, http.StatusForbidden, "user has no accounts")
		return
	case 1:
		user := login.Users[0]

		if !c.configuration.Stripe.BillingEnabled {
			token, err := c.generateToken(login.LoginId, user.UserId, user.AccountId, true)
			if err != nil {
				c.wrapAndReturnError(ctx, err, http.StatusInternalServerError, "could not generate JWT")
				return
			}
			// Return their account token.
			ctx.JSON(map[string]interface{}{
				"token": token,
			})
			return
		}

		repo := repository.NewRepositoryFromSession(user.UserId, user.AccountId, c.db)
		subscription, err := repo.GetActiveSubscription(c.getContext(ctx))
		if err != nil {
			c.wrapPgError(ctx, err, "failed to get active subscription")
			return
		}

		var subscriptionIsActive bool
		if subscription == nil {
			subscriptionIsActive = false
		} else {
			switch subscription.Status {
			case stripe.SubscriptionStatusActive,
				stripe.SubscriptionStatusTrialing:
				subscriptionIsActive = true
			case stripe.SubscriptionStatusPastDue,
				stripe.SubscriptionStatusUnpaid,
				stripe.SubscriptionStatusCanceled,
				stripe.SubscriptionStatusIncomplete,
				stripe.SubscriptionStatusIncompleteExpired:
				subscriptionIsActive = false
			default:
				sentry.CaptureMessage(fmt.Sprintf("invalid subscription status: %s", subscription.Status))
				c.returnError(ctx, http.StatusNotImplemented, "invalid subscription status, create a github issue")
				return
			}
		}

		token, err := c.generateToken(login.LoginId, user.UserId, user.AccountId, subscriptionIsActive)
		if err != nil {
			c.wrapAndReturnError(ctx, err, http.StatusInternalServerError, "could not generate JWT")
			return
		}

		result := map[string]interface{}{
			"token": token,
		}

		if !subscriptionIsActive {
			result["nextUrl"] = "/account/subscribe"
		}

		ctx.JSON(result)
	default:
		// If the login has more than one user then we want to generate a temp
		// JWT that will only grant them access to API endpoints not specific to
		// an account.
		token, err := c.generateToken(login.LoginId, 0, 0, true)
		if err != nil {
			c.wrapAndReturnError(ctx, err, http.StatusInternalServerError, "could not generate JWT")
			return
		}

		ctx.JSON(map[string]interface{}{
			"token": token,
			"users": login.Users,
		})
	}
}

func (c *Controller) validateLogin(email, password string) error {
	// TODO (elliotcourant) Add some email format validation here.
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}

	return nil
}

func (c *Controller) generateToken(loginId, userId, accountId uint64, subscriptionActive bool) (string, error) {
	now := time.Now()
	claims := &HarderClaims{
		LoginId:   loginId,
		UserId:    userId,
		AccountId: accountId,
		StandardClaims: jwt.StandardClaims{
			Audience: []string{
				c.configuration.APIDomainName,
			},
			ExpiresAt: now.Add(31 * 24 * time.Hour).Unix(),
			Id:        "",
			IssuedAt:  now.Unix(),
			Issuer:    c.configuration.APIDomainName,
			NotBefore: now.Unix(),
			Subject:   "monetr",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signedToken, err := token.SignedString([]byte(c.configuration.JWT.LoginJwtSecret))
	if err != nil {
		return "", errors.Wrap(err, "failed to sign JWT")
	}

	return signedToken, nil
}
