package domain

import (
	"errors"
	"fmt"
)

type ModelExecution string

const (
	ModelExecutionLocal  ModelExecution = "local"
	ModelExecutionCloud  ModelExecution = "cloud"
	ModelExecutionCenter ModelExecution = "center"
)

type ScoreRoute string

const (
	ScoreRouteLoopback     ScoreRoute = "loopback"
	ScoreRouteHighlightAPI ScoreRoute = "highlight_api"
	ScoreRouteDirect       ScoreRoute = "direct"
	ScoreRouteCenter       ScoreRoute = "center"
)

type ExecutionClient string

const (
	ExecutionClientUser        ExecutionClient = "user"
	ExecutionClientDevelopment ExecutionClient = "development"
)

var (
	ErrInvalidModelExecution  = errors.New("invalid highlight model execution")
	ErrInvalidScoreRoute      = errors.New("invalid highlight score route")
	ErrInvalidExecutionClient = errors.New("invalid highlight execution client")
	ErrInvalidExecutionRoute  = errors.New("invalid highlight execution and score route combination")
)

func ParseModelExecution(value string) (ModelExecution, error) {
	execution := ModelExecution(value)
	switch execution {
	case ModelExecutionLocal, ModelExecutionCloud, ModelExecutionCenter:
		return execution, nil
	default:
		return "", ErrInvalidModelExecution
	}
}

func ParseScoreRoute(value string) (ScoreRoute, error) {
	route := ScoreRoute(value)
	switch route {
	case ScoreRouteLoopback, ScoreRouteHighlightAPI, ScoreRouteDirect, ScoreRouteCenter:
		return route, nil
	default:
		return "", ErrInvalidScoreRoute
	}
}

func ParseExecutionClient(value string) (ExecutionClient, error) {
	client := ExecutionClient(value)
	switch client {
	case ExecutionClientUser, ExecutionClientDevelopment:
		return client, nil
	default:
		return "", ErrInvalidExecutionClient
	}
}

// ValidateExecutionRoute permits only the explicit client/execution/route
// combinations in the protocol. Empty or unknown values are never inferred
// from provider, base URL or another field.
func ValidateExecutionRoute(client ExecutionClient, execution ModelExecution, route ScoreRoute) error {
	parsedClient, err := ParseExecutionClient(string(client))
	if err != nil {
		return err
	}
	parsedExecution, err := ParseModelExecution(string(execution))
	if err != nil {
		return err
	}
	parsedRoute, err := ParseScoreRoute(string(route))
	if err != nil {
		return err
	}
	valid := parsedExecution == ModelExecutionLocal && parsedRoute == ScoreRouteLoopback ||
		parsedExecution == ModelExecutionCenter && parsedRoute == ScoreRouteCenter
	switch parsedClient {
	case ExecutionClientUser:
		valid = valid || parsedExecution == ModelExecutionCloud && parsedRoute == ScoreRouteHighlightAPI
	case ExecutionClientDevelopment:
		valid = valid || parsedExecution == ModelExecutionCloud && parsedRoute == ScoreRouteDirect
	}
	if !valid {
		return fmt.Errorf("%w: client=%q execution=%q route=%q", ErrInvalidExecutionRoute, parsedClient, parsedExecution, parsedRoute)
	}
	return nil
}
