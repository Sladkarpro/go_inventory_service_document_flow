package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go_warehouse_service/internal/client"
	"go_warehouse_service/internal/config"
	"go_warehouse_service/internal/crypto"
	"go_warehouse_service/internal/handler"
	"go_warehouse_service/internal/messaging/usersync"
	"go_warehouse_service/internal/middleware"
	"go_warehouse_service/internal/repository"
	"go_warehouse_service/internal/service"
)

type App struct {
	router           *chi.Mux
	cfg              *config.Config
	userSyncConsumer *usersync.Consumer
	syncService      *service.SyncService
}

type Handlers struct {
	Item       *handler.ItemHandler
	Holding    *handler.HoldingHandler
	Warehouse  *handler.WarehouseHandler
	Credential *handler.CredentialHandler
	Import     *handler.ImportHandler
	Report     *handler.ReportHandler
}

func NewApp(cfg *config.Config, db *pgxpool.Pool) (*App, error) {
	// Сначала то, что может упасть, потом слои. Читать ключ JWT после сборки
	// половины графа зависимостей — значит узнавать о неверном пути к ключу
	// последним, уже открыв соединения.
	authMw, err := middleware.NewAuthMiddleware(cfg.JWTPublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to init auth middleware: %w", err)
	}

	// ⚠ Отсутствие ключа шифрования НЕ мешает сервису подняться: инвентаризацию
	// можно вести и без паролей от техники. Но ручки паролей будут отвечать
	// ошибкой, а не писать секреты открытым текстом.
	var cipher *crypto.Cipher
	if cfg.CredentialsKey == "" {
		slog.Warn("WAREHOUSE_CREDENTIALS_KEY не задан — " +
			"работа с паролями от техники будет недоступна")
	} else {
		cipher, err = crypto.New(cfg.CredentialsKey)
		if err != nil {
			// Кривой ключ — это ошибка настройки, и молча её проглатывать нельзя:
			// иначе пароли окажутся недоступны, а причина останется неизвестной.
			return nil, fmt.Errorf("WAREHOUSE_CREDENTIALS_KEY: %w", err)
		}
	}

	userRepo := repository.NewUserRepository(db, cfg.Clock)
	deptRepo := repository.NewDepartmentRepository(db, cfg.Clock)
	itemRepo := repository.NewItemRepository(db)
	holdingRepo := repository.NewHoldingRepository(db)
	warehouseRepo := repository.NewWarehouseRepository(db)
	credRepo := repository.NewCredentialRepository(db)
	reportRepo := repository.NewReportRepository(db)

	symfonyClient := client.NewSymfonyClient(cfg)
	syncSvc := service.NewSyncService(symfonyClient, userRepo, deptRepo)

	itemSvc := service.NewItemService(itemRepo, holdingRepo)
	holdingSvc := service.NewHoldingService(holdingRepo, cfg.Clock)
	warehouseSvc := service.NewWarehouseService(warehouseRepo)
	credSvc := service.NewCredentialService(credRepo, cipher, cfg.Clock)
	reportSvc := service.NewReportService(reportRepo)
	importSvc := service.NewImportService(db, itemRepo, holdingRepo, userRepo, deptRepo, cfg.Clock)

	h := Handlers{
		Item:       handler.NewItemHandler(itemSvc),
		Holding:    handler.NewHoldingHandler(holdingSvc),
		Warehouse:  handler.NewWarehouseHandler(warehouseSvc),
		Credential: handler.NewCredentialHandler(credSvc),
		Import:     handler.NewImportHandler(importSvc),
		Report:     handler.NewReportHandler(reportSvc, cfg.Clock),
	}

	return &App{
		router:           setupRouter(h, authMw, syncSvc),
		cfg:              cfg,
		userSyncConsumer: usersync.NewConsumer(cfg, userRepo),
		syncService:      syncSvc,
	}, nil
}

func (a *App) Run() error {
	addr := fmt.Sprintf(":%s", a.cfg.Port)
	appCtx, stopBackground := context.WithCancel(context.Background())
	var backgroundWG sync.WaitGroup

	// Стартовая загрузка справочников.
	// ⚠ Без неё реплика users пуста, и первый импорт не сматчит ни одного
	// из 163 сотрудников: события RabbitMQ описывают только изменения,
	// а всё опубликованное до появления нашей очереди потеряно.
	backgroundWG.Add(1)
	go func() {
		defer backgroundWG.Done()
		a.syncService.Bootstrap(appCtx)
	}()

	backgroundWG.Add(1)
	go func() {
		defer backgroundWG.Done()
		a.syncService.RunPeriodic(appCtx, time.Hour)
	}()

	if a.userSyncConsumer != nil {
		backgroundWG.Add(1)
		go func() {
			defer backgroundWG.Done()
			if err := a.userSyncConsumer.Run(appCtx); err != nil {
				slog.Error("Ошибка RabbitMQ consumer синхронизации пользователей", "error", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: a.router,
		// Импорт xlsx длиннее обычного запроса, отсюда увеличенные таймауты.
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Ошибка старта HTTP-сервера", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("Получен сигнал завершения, начинаем graceful shutdown...")
	stopBackground()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("ошибка при остановке сервера: %w", err)
	}

	backgroundDone := make(chan struct{})
	go func() {
		backgroundWG.Wait()
		close(backgroundDone)
	}()

	select {
	case <-backgroundDone:
	case <-ctx.Done():
		slog.Warn("Не все фоновые процессы завершились до таймаута")
	}

	slog.Info("HTTP-сервер успешно остановлен")
	return nil
}
