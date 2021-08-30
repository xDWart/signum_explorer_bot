package main

import (
	"github.com/joho/godotenv"
	"github.com/xDWart/signum-explorer-bot/internal"
	"go.uber.org/zap"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	loggerConfig := zap.NewDevelopmentConfig()
	loggerConfig.DisableStacktrace = true
	loggerConfig.DisableCaller = true
	zapLogger, _ := loggerConfig.Build()
	defer zapLogger.Sync()
	logger := zapLogger.Sugar()

	err := godotenv.Load()
	if err != nil {
		logger.Infof("Using environment variables from container environment")
	} else {
		logger.Infof("Using environment variables from .env file")
	}

	bot := internal.InitTelegramBot(logger)

	var gracefulStop = make(chan os.Signal)
	signal.Notify(gracefulStop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-gracefulStop
		logger.Infof("Caught system sig: %+v", sig)
		bot.Shutdown()
	}()

	bot.Wait()
}
