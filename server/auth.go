package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/argon2"
	"gorm.io/gorm"
)

type UserType uint8

var (
	// Assume watcharr user if none of these...
	JELLYFIN_USER UserType = 1
)

// uniqueIndex applied between Username and UserType, so same usernames can exist, but only with different types.
// This is incase different users with same name from different services try to signup.
type User struct {
	GormModel
	Username string `gorm:"uniqueIndex:usr_name_to_type;not null" json:"username" binding:"required"`
	Password string `gorm:"not null" json:"password" binding:"required"`
	// The type of user/which auth service they originate from.
	// Empty if from Watcharr, or the name of the service (eg. jellyfin)
	Type UserType `gorm:"uniqueIndex:usr_name_to_type;not null;default:0" json:"type"`
	// ID of user from the third party service, this will be used purely for lookup of user at signin.
	ThirdPartyID string `json:"-"`
	Watched      []Watched
}

type JellyfinAuth struct {
	Username string `json:"Username"`
	Pw       string `json:"Pw"`
}

type JellyfinAuthResponse struct {
	User struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	} `json:"User"`
}

type AuthResponse struct {
	Token string `json:"token"`
}

type ArgonParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

type TokenClaims struct {
	UserID   uint   `json:"userId"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// Auth middleware
func AuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		slog.Debug("AuthRequired middleware hit")
		atoken := c.GetHeader("Authorization")
		// Make sure auth header isn't empty
		if atoken == "" {
			slog.Warn("Returning 401, Authorization header not provided")
			c.AbortWithStatus(401)
			return
		}
		// Parse token
		token, err := jwt.ParseWithClaims(atoken, &TokenClaims{}, func(token *jwt.Token) (interface{}, error) {
			return []byte(os.Getenv("JWT_SECRET")), nil
		})
		if err != nil {
			slog.Error("AuthRequired failed to parse token", "error", err)
			c.AbortWithStatus(401)
			return
		}
		// If token is valid, go to next handler
		if claims, ok := token.Claims.(*TokenClaims); ok && token.Valid {
			slog.Debug("Token is valid", "userId", claims.UserID, "username", claims.Username)
			c.Set("userId", claims.UserID)
			c.Next()
		} else {
			slog.Error("Token is **not** valid")
			c.AbortWithStatus(401)
			return
		}
	}
}

func register(user *User, db *gorm.DB) (AuthResponse, error) {
	slog.Info("A user is registering", "username", user.Username)
	hash, err := hashPassword(user.Password, &ArgonParams{
		memory:      64 * 1024,
		iterations:  3,
		parallelism: 2,
		saltLength:  16,
		keyLength:   32,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Update user obj to replace the plaintext pass with hash
	user.Password = hash

	res := db.Create(&user)
	if res.Error != nil {
		// If error is because unique contraint failed.. user already exists
		if strings.Contains(res.Error.Error(), "UNIQUE") {
			slog.Error("Registration failed", "error", res.Error.Error(), "error_pretty", "User already exists")
			return AuthResponse{}, errors.New("User already exists")
		}
		slog.Error("Registration failed", "error", err, "error_pretty", "Watcharr does not know why this failed, assume database operation failed")
		return AuthResponse{}, errors.New("unknown error")
	}

	// Gorm fills our user obj with the ID from db after insert,
	// just ensure it actually has.
	if user.ID == 0 {
		slog.Error("user.ID not filled out after registration", "userId", user.ID)
		return AuthResponse{}, errors.New("failed to get user id, try login")
	}

	token, err := signJWT(user)
	if err != nil {
		slog.Error("Registration: Failed to sign new jwt", "error", err)
		return AuthResponse{}, errors.New("failed to get auth token")
	}
	return AuthResponse{Token: token}, nil
}

func login(user *User, db *gorm.DB) (AuthResponse, error) {
	slog.Debug("A User Is Logging In", "username", user.Username)
	dbUser := new(User)
	res := db.Where("username = ? AND (type IS NULL OR type = 0)", user.Username).Take(&dbUser)
	if res.Error != nil {
		slog.Error("Failed to select user from database for login", "error", res.Error)
		return AuthResponse{}, errors.New("User does not exist")
	}

	match, err := compareHash(user.Password, dbUser.Password)
	if err != nil {
		slog.Error("Failed to compare pass to hash for login", "error", err)
		return AuthResponse{}, errors.New("failed to login")
	}
	if !match {
		slog.Error("User failed to provide correct password for login", "hash_matched", match)
		return AuthResponse{}, errors.New("incorrect details")
	}

	token, err := signJWT(dbUser)
	if err != nil {
		slog.Error("Failed to sign new jwt", "error", err)
		return AuthResponse{}, errors.New("failed to get auth token")
	}
	return AuthResponse{Token: token}, nil
}

func loginJellyfin(user *User, db *gorm.DB) (AuthResponse, error) {
	jellyfinHost := os.Getenv("JELLYFIN_HOST")
	if jellyfinHost == "" {
		slog.Error("Request made to login via Jellyfin, but JELLYFIN_HOST environment variable is not set.")
		return AuthResponse{}, errors.New("jellyfin login not enabled")
	}

	base, err := url.Parse(jellyfinHost + "/Users/AuthenticateByName")
	if err != nil {
		slog.Error("Failed to parse AuthenticateByName api endpoint url", "error", err.Error())
		return AuthResponse{}, errors.New("failed to parse api uri")
	}

	// Marshall struct as json
	usrJSON, err := json.Marshal(JellyfinAuth{Username: user.Username, Pw: user.Password})
	if err != nil {
		slog.Error("Error marshalling JellyfinAuth JSON", "error", err.Error())
		return AuthResponse{}, errors.New("failed to marshal json")
	}
	// Run auth request
	// res, err := http.Post(base.String(), "application/json", bytes.NewBuffer(usrJSON))
	client := &http.Client{}
	req, err := http.NewRequest("POST", base.String(), bytes.NewBuffer(usrJSON))
	if err != nil {
		slog.Error("Creating request to jellyfin for auth failed", "error", err)
		return AuthResponse{}, errors.New("request failed")
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("X-Emby-Authorization", "MediaBrowser Client=\"Watcharr\", Device=\"HTTP\", DeviceId=\"WatcharrFor"+user.Username+"\", Version=\"10.8.0\"")
	res, err := client.Do(req)
	if err != nil {
		slog.Error("making request to jellyfin for auth failed", "error", err)
		return AuthResponse{}, errors.New("request failed")
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		slog.Error("Error reading jellyfin auth response", "error", err.Error())
		return AuthResponse{}, err
	}
	if res.StatusCode != 200 {
		slog.Error("Jellyfin auth non 200 status code", "status_code", res.StatusCode, "error", string(body))
		return AuthResponse{}, errors.New("incorrect details")
	}
	// Process auth response
	resp := new(JellyfinAuthResponse)
	err = json.Unmarshal([]byte(body), &resp)
	if err != nil {
		return AuthResponse{}, errors.New("failed to process response")
	}
	if resp.User.ID == "" {
		return AuthResponse{}, errors.New("jellyfin returned empty user id")
	}

	dbUser := new(User)
	dbRes := db.Where("third_party_id = ?", resp.User.ID).Take(&dbUser)
	if dbRes.Error != nil {
		if errors.Is(dbRes.Error, gorm.ErrRecordNotFound) {
			// Record not found, so we should create the user
			// dbUser will be empty, so we can just reuse it for this purpose.
			dbUser.ThirdPartyID = resp.User.ID
			dbUser.Username = resp.User.Name
			dbUser.Type = JELLYFIN_USER

			dbRes = db.Create(&dbUser)
			if dbRes.Error != nil {
				slog.Error("Failed to create new user in db from jellyfin response", "error", err.Error())
				return AuthResponse{}, errors.New("failed to create new user from jellyfin")
			}
		} else {
			return AuthResponse{}, errors.New("error locating user in db")
		}
	}

	token, err := signJWT(dbUser)
	if err != nil {
		slog.Error("Failed to sign new (jellyfin login) jwt", "error", err)
		return AuthResponse{}, errors.New("failed to get auth token")
	}
	return AuthResponse{Token: token}, nil
}

func signJWT(user *User) (token string, err error) {
	// Create new jwt with claim data
	jwt := jwt.NewWithClaims(jwt.SigningMethodHS256, TokenClaims{
		user.ID,
		user.Username,
		jwt.RegisteredClaims{
			// ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt: jwt.NewNumericDate(time.Now()),
			Issuer:   "watcharr",
		},
	})

	// Sign and get the complete encoded token as a string using the secret
	return jwt.SignedString([]byte(os.Getenv("JWT_SECRET")))
}

func hashPassword(password string, p *ArgonParams) (encodedHash string, err error) {
	salt, err := generateRandomBytes(p.saltLength)
	if err != nil {
		return "", err
	}

	hash := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, p.parallelism, p.keyLength)

	// Base64 encode the salt and hashed password.
	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)

	// Format hash in standard way.
	encodedHash = fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memory, p.iterations, p.parallelism, b64Salt, b64Hash)

	return encodedHash, nil
}

func generateRandomBytes(n uint32) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func compareHash(password, encodedHash string) (match bool, err error) {
	// Extract the parameters, salt and derived key from the encoded password
	// hash.
	p, salt, hash, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}

	// Derive the key from the other password using the same parameters.
	otherHash := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, p.parallelism, p.keyLength)

	// Check that the contents of the hashed passwords are identical. Note
	// that we are using the subtle.ConstantTimeCompare() function for this
	// to help prevent timing attacks.
	if subtle.ConstantTimeCompare(hash, otherHash) == 1 {
		return true, nil
	}
	return false, nil
}

func decodeHash(encodedHash string) (p *ArgonParams, salt, hash []byte, err error) {
	vals := strings.Split(encodedHash, "$")
	if len(vals) != 6 {
		return nil, nil, nil, errors.New("the encoded hash is not in the correct format")
	}

	var version int
	_, err = fmt.Sscanf(vals[2], "v=%d", &version)
	if err != nil {
		return nil, nil, nil, err
	}
	if version != argon2.Version {
		return nil, nil, nil, errors.New("incompatible version of argon2")
	}

	p = &ArgonParams{}
	_, err = fmt.Sscanf(vals[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism)
	if err != nil {
		return nil, nil, nil, err
	}

	salt, err = base64.RawStdEncoding.Strict().DecodeString(vals[4])
	if err != nil {
		return nil, nil, nil, err
	}
	p.saltLength = uint32(len(salt))

	hash, err = base64.RawStdEncoding.Strict().DecodeString(vals[5])
	if err != nil {
		return nil, nil, nil, err
	}
	p.keyLength = uint32(len(hash))

	return p, salt, hash, nil
}
