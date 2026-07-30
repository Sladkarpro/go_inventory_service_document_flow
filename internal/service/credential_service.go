package service

import (
	"context"
	"log/slog"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/crypto"
	"go_warehouse_service/internal/helper"
	"go_warehouse_service/internal/model"
	"go_warehouse_service/internal/repository"
)

// CredentialService — доступы к технике.
//
// Три требования, не подлежащие упрощению:
//  1. Секрет хранится зашифрованным (AES-256-GCM), ключ только в окружении.
//  2. Секрет НЕ ОТДАЁТСЯ В СПИСКАХ — только отдельным Reveal, только админу.
//  3. Каждый Reveal пишется в журнал: это единственный способ ответить,
//     кто видел пароль от коммутатора до того, как сеть легла, и что менять
//     после увольнения.
type CredentialService struct {
	creds  *repository.CredentialRepository
	cipher *crypto.Cipher
	clock  helper.Clock
}

func NewCredentialService(
	creds *repository.CredentialRepository, cipher *crypto.Cipher, clk helper.Clock,
) *CredentialService {
	return &CredentialService{creds: creds, cipher: cipher, clock: clk}
}

func (s *CredentialService) requireCipher() error {
	if s.cipher == nil {
		// Сервис поднялся без ключа: читать список можно, заводить и показывать
		// пароли — нет. Молча писать открытым текстом недопустимо.
		return apperr.New(apperr.CodeCredentialsKeyMissing, "credentials key not configured")
	}
	return nil
}

func (s *CredentialService) ListByHolding(ctx context.Context, holdingID int64) ([]repository.CredentialView, error) {
	// ⚠ Список доступов — АДМИНСКАЯ ручка, хотя пароль в ней и не отдаётся.
	//
	// Раньше здесь стояло requireRead, и наблюдатель, МОЛ и начальник отдела
	// получали login и url админ-панелей устройств (замерено: HTTP 200,
	// login=root, url=http://10.0.0.1). Сам секрет не утекал, только флаг
	// hasSecret, — но логин и адрес админки это половина доступа, остаётся
	// подобрать пароль. Все прочие методы этого ресурса (создать, править,
	// удалить, reveal) требуют администратора, и чтение было единственным
	// исключением. Решение владельца 29.07.2026 — закрыть.
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	// Репозиторий не читает secret_cipher вовсе — утечь тут нечему.
	return s.creds.ListByHolding(ctx, holdingID)
}

type CredentialCreate struct {
	HoldingID int64
	Kind      string
	Title     string
	URL       *string
	Login     *string
	Secret    string
	Note      *string
}

func (s *CredentialService) Create(ctx context.Context, in CredentialCreate) (*repository.CredentialView, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := s.requireCipher(); err != nil {
		return nil, err
	}
	if !model.ValidCredentialKind(in.Kind) {
		return nil, apperr.New(apperr.CodeCredentialKindInvalid, "invalid credential kind")
	}
	if model.CleanText(in.Title) == "" {
		return nil, apperr.New(apperr.CodeValidation, "title required")
	}
	if in.Secret == "" {
		return nil, apperr.New(apperr.CodeSecretRequired, "secret required")
	}

	envelope, version, err := s.cipher.Encrypt(in.Secret)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeConflict, "encrypt failed", err)
	}

	return s.creds.Create(ctx, repository.CredentialInput{
		HoldingID:    in.HoldingID,
		Kind:         in.Kind,
		Title:        in.Title,
		URL:          in.URL,
		Login:        in.Login,
		SecretCipher: &envelope,
		KeyVersion:   version,
		Note:         in.Note,
	})
}

type CredentialPatch struct {
	Kind  *string
	Title *string
	URL   *string
	Login *string
	Note  *string
	// Secret непустой — смена пароля. Она же ротация: доступ уходит
	// из отчёта «что менять после увольнения».
	Secret *string
}

func (s *CredentialService) Update(ctx context.Context, id int64, in CredentialPatch) (*repository.CredentialView, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if in.Kind != nil && !model.ValidCredentialKind(*in.Kind) {
		return nil, apperr.New(apperr.CodeCredentialKindInvalid, "invalid credential kind")
	}

	upd := repository.CredentialUpdate{
		Kind: in.Kind, Title: in.Title, URL: in.URL, Login: in.Login, Note: in.Note,
	}
	if in.Secret != nil {
		if err := s.requireCipher(); err != nil {
			return nil, err
		}
		if *in.Secret == "" {
			return nil, apperr.New(apperr.CodeSecretRequired, "secret required")
		}
		envelope, version, err := s.cipher.Encrypt(*in.Secret)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeConflict, "encrypt failed", err)
		}
		upd.SecretCipher = &envelope
		upd.KeyVersion = &version
	}
	return s.creds.Update(ctx, id, upd, s.clock.Now())
}

// Delete удаляет доступ, а просмотренный — затирает.
// Возвращает wiped=true, если запись осталась ради журнала.
func (s *CredentialService) Delete(ctx context.Context, id int64) (bool, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return false, err
	}
	return s.creds.Delete(ctx, id, s.clock.Now())
}

// Reveal — ЕДИНСТВЕННЫЙ путь, которым секрет покидает сервис.
//
// Только администратор инвентаризации. Журнал пишется ДО возврата значения:
// если запись в журнал не удалась, секрет не отдаётся. Иначе появился бы
// способ посмотреть пароль, не оставив следа, — а весь смысл журнала в том,
// что следа избежать нельзя.
func (s *CredentialService) Reveal(ctx context.Context, id int64, ip *string) (string, error) {
	user, err := requireAdmin(ctx)
	if err != nil {
		return "", err
	}
	if err := s.requireCipher(); err != nil {
		return "", err
	}

	envelope, _, err := s.creds.GetSecret(ctx, id)
	if err != nil {
		return "", err
	}

	secret, err := s.cipher.Decrypt(envelope)
	if err != nil {
		// В лог уходит ошибка расшифровки, но НЕ конверт и НЕ ключ.
		slog.Error("не удалось расшифровать доступ", "credential_id", id, "error", err)
		return "", apperr.Wrap(apperr.CodeConflict, "decrypt failed", err)
	}

	if err := s.creds.LogView(ctx, id, user.ID, ip, s.clock.Now()); err != nil {
		slog.Error("не удалось записать просмотр пароля в журнал",
			"credential_id", id, "user_id", user.ID, "error", err)
		return "", err
	}

	slog.Info("просмотр пароля",
		"credential_id", id, "user_id", user.ID, "username", user.Username)
	return secret, nil
}

// ToRotate — что менять после увольнения: доступы, которые человек видел
// и которые с тех пор не ротировали.
//
// Чего этот отчёт НЕ решает, и это надо сказать вслух: он не мешает записать
// пароли до ухода, не отзывает доступы автоматически и не защищает от
// злоупотребления тем, кто имел право смотреть. Это организационные меры.
func (s *CredentialService) ToRotate(ctx context.Context, userID *int64) ([]model.CredentialToRotateRow, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return s.creds.ToRotate(ctx, userID)
}
