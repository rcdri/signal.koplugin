//go:build !demo

package main

import (
	"errors"

	"github.com/google/uuid"
)

var demoMe = uuid.Nil
var demoNames = map[uuid.UUID]string{}

var errNoDemo = errors.New("not a demo build")

func startDemo(*App) bool                                 { return false }
func demoLogin(*App) error                                { return errNoDemo }
func demoLogout(*App) error                               { return errNoDemo }
func demoSend(*App, string, string, string) (*Msg, error) { return nil, errNoDemo }
func demoEdit(*App, string, string, string) error         { return errNoDemo }
func demoContacts(*App) []Contact                         { return []Contact{} }
func demoMedia(*Msg, int, string) error                   { return errNoDemo }
