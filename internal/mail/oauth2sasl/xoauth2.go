package oauth2sasl

import "github.com/emersion/go-sasl"

const mechanism = "XOAUTH2"

type client struct {
	username string
	token    string
}

func NewClient(username, token string) sasl.Client {
	return &client{username: username, token: token}
}

func (c *client) Start() (string, []byte, error) {
	return mechanism, []byte("user=" + c.username + "\x01auth=Bearer " + c.token + "\x01\x01"), nil
}

func (c *client) Next(challenge []byte) ([]byte, error) {
	return nil, sasl.ErrUnexpectedServerChallenge
}
