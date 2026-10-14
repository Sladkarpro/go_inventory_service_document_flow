package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"go_warehouse_service/internal/helper"
)

type Config struct {
	Env              string
	Port             string
	JWTPublicKeyPath string

	RabbitMQDSN      string
	RabbitMQExchange string
	UserSyncQueue    string

	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBName     string

	// Symfony internal API (справочники пользователей и подразделений)
	SymfonyInternalApiUrl string
	SymfonyInternalApiKey string

	// CredentialsKey — ключ AES-256-GCM для паролей от техники, base64, 32 байта.
	// Живёт ТОЛЬКО в переменной окружения: в базе его быть не должно, иначе бэкап
	// содержит и замок, и ключ. Потеря = безвозвратная потеря всех паролей.
	CredentialsKey string

	// Clock даёт текущее время в UTC, усечённое до секунды (колонки TIMESTAMPTZ(0)).
	Clock helper.Clock
}

func Load() *Config {
	if err := godotenv.Load(); err != nil {
		slog.Warn("Предупреждение: .env файл не найден, используются системные переменные окружения")
	}

	c := &Config{
		Env: getEnv("ENV", "local"),
		// 8085 занят kanban_service, 8086 — notification_service
		// (analytics_platform/docker_env/nginx/config/default.conf).
		Port:             getEnv("SERVER_PORT", "8087"),
		JWTPublicKeyPath: getEnv("JWT_PUBLIC_KEY_PATH", "config/jwt/public.pem"),

		RabbitMQDSN:      getEnv("RABBITMQ_TRANSPORT_DSN", "amqp://guest:guest@rabbitmq:5672/"),
		RabbitMQExchange: getEnv("RABBITMQ_EVENTS_EXCHANGE", "events"),
		// ⚠ Точка в имени обязательна. Политика dead-letter монолита
		// (docker_env/rabbitmq/definitions.json, dlx-go) выбирает очереди
		// шаблоном ^(kanban|notification|warehouse)\. — под «warehouse_user_sync»
		// с подчёркиванием она не попадает, и отравленные сообщения после
		// Nack(false,false) отбрасываются молча, без DLQ.
		UserSyncQueue: getEnv("RABBITMQ_USER_SYNC_QUEUE", "warehouse.user_sync"),

		DBHost:     getEnv("DB_HOST", "localhost"),
		DBPort:     getEnvAsInt("DB_PORT", 5434),
		DBUser:     getEnv("DB_USER", ""),
		DBPassword: getEnv("DB_PASSWORD", ""),
		DBName:     getEnv("DB_NAME", ""),

		SymfonyInternalApiUrl: getEnv("SYMFONY_INTERNAL_API_URL", ""),
		SymfonyInternalApiKey: getEnv("SYMFONY_INTERNAL_API_KEY", ""),

		CredentialsKey: getEnv("WAREHOUSE_CREDENTIALS_KEY", ""),
	}

	c.Clock = helper.NewClock()

	return c
}

func ConnectDB(conf *Config) (*pgxpool.Pool, error) {
	// Без параметра timezone: колонки TIMESTAMPTZ, на стороне Go работаем в UTC.
	connStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		conf.DBHost,
		conf.DBPort,
		conf.DBUser,
		conf.DBPassword,
		conf.DBName,
	)

	pool, err := pgxpool.New(context.Background(), connStr)
	if err != nil {
		return nil, err
	}

	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return nil, err
	}

	return pool, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvAsInt(key string, fallback int) int {
	valueStr := getEnv(key, "")
	if value, err := strconv.Atoi(valueStr); err == nil {
		return value
	}
	return fallback
}

// Now возвращает текущий момент (UTC, усечённый до секунды).
func (c *Config) Now() time.Time {
	return c.Clock.Now()
}
