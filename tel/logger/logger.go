// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package logger

import (
	"log/slog"
	"os"
)

func InitLogging() {
	logLevel := os.Getenv("LOG_LEVEL")
	if 0 == len(logLevel) {
		logLevel = "INFO"
	}

	level := new(slog.LevelVar)
	level.UnmarshalText([]byte(logLevel))

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})))

	slog.Info("Start logger with level: " + logLevel)
}

// func init() {
// 	InitLogging()
// }
