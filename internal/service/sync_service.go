package service

import (
	"context"
	"log/slog"
	"time"

	"go_warehouse_service/internal/client"
	"go_warehouse_service/internal/repository"
)

// SyncService держит реплики монолита в актуальном состоянии.
//
// ⚠ ТРИ СЦЕНАРИЯ ОТКАЗА ДЕГРАДИРУЮТ МОЛЧА: очередь встала — реплика отстаёт;
// Symfony недоступна — показываем старые названия; сервис лежал — очередь копит.
// Экран при этом не ломается и ничего не сообщает. Поэтому /healthz обязан
// отдавать возраст последней успешной синхронизации: это единственный сигнал.
type SyncService struct {
	symfony     *client.SymfonyClient
	users       *repository.UserRepository
	departments *repository.DepartmentRepository
}

func NewSyncService(
	symfony *client.SymfonyClient,
	users *repository.UserRepository,
	departments *repository.DepartmentRepository,
) *SyncService {
	return &SyncService{symfony: symfony, users: users, departments: departments}
}

// Bootstrap загружает справочники при старте.
//
// Ошибки НЕ фатальны: сервис обязан подниматься и при недоступной Symfony,
// иначе перезапуск монолита ронял бы и нас. Но каждая неудача громко пишется
// в лог, а её последствия видны в /healthz.
func (s *SyncService) Bootstrap(ctx context.Context) {
	count, err := s.users.Count(ctx)
	if err != nil {
		slog.Error("не удалось прочитать реплику пользователей", "error", err)
	}

	if err := s.SyncUsers(ctx); err != nil {
		if count == 0 {
			// Реплика пуста И загрузить её не удалось: первый импорт сматчит
			// НОЛЬ сотрудников из 163, и все строки лягут с ФИО строкой.
			slog.Error("РЕПЛИКА ПОЛЬЗОВАТЕЛЕЙ ПУСТА и не загрузилась — "+
				"импорт не сможет связать сотрудников по ФИО", "error", err)
		} else {
			slog.Warn("не удалось обновить реплику пользователей, работаем на прежних данных",
				"error", err, "known_users", count)
		}
	}

	if err := s.SyncDepartments(ctx); err != nil {
		slog.Warn("не удалось обновить справочник подразделений, "+
			"показываем последние сохранённые названия", "error", err)
	}
}

func (s *SyncService) SyncUsers(ctx context.Context) error {
	users, err := s.symfony.FetchAllUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		slog.Warn("монолит вернул пустой список пользователей")
		return nil
	}
	if err := s.users.UpsertUsers(ctx, users); err != nil {
		return err
	}
	slog.Info("реплика пользователей обновлена", "count", len(users))
	return nil
}

func (s *SyncService) SyncDepartments(ctx context.Context) error {
	deps, err := s.symfony.FetchDepartments(ctx)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		return nil
	}
	if err := s.departments.Upsert(ctx, deps); err != nil {
		return err
	}
	slog.Info("справочник подразделений обновлён", "count", len(deps))
	return nil
}

// RunPeriodic обновляет справочники по таймеру.
// Подразделения событий не имеют вовсе, пользователи — имеют, но события
// описывают только изменения, и периодическая сверка чинит расхождение,
// накопленное за время простоя consumer'а.
func (s *SyncService) RunPeriodic(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.SyncDepartments(ctx); err != nil {
				slog.Warn("периодическая синхронизация подразделений не удалась", "error", err)
			}
			if err := s.SyncUsers(ctx); err != nil {
				slog.Warn("периодическая синхронизация пользователей не удалась", "error", err)
			}
		}
	}
}

// LastSyncAge — возраст последней успешной синхронизации, для /healthz.
func (s *SyncService) LastSyncAge(ctx context.Context) *time.Duration {
	age, err := s.users.LastSyncAge(ctx)
	if err != nil {
		return nil
	}
	return age
}
