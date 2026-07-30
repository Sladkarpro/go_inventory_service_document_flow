package repository

import (
	"context"
	"errors"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/model"
)

// CredentialRepository хранит доступы к технике.
//
// ⚠ ГЛАВНОЕ ПРАВИЛО СЛОЯ: secret_cipher не покидает репозиторий нигде, кроме
// GetSecret. Списочные выборки его не читают ВООБЩЕ — не «читают и отбрасывают»,
// а не включают в SELECT. Так секрет не может утечь через логирование строки,
// отладочный дамп или забытое поле в DTO.
type CredentialRepository struct {
	db *pgxpool.Pool
}

func NewCredentialRepository(db *pgxpool.Pool) *CredentialRepository {
	return &CredentialRepository{db: db}
}

// listColumns — БЕЗ secret_cipher. Менять с осторожностью.
const credentialListColumns = `c.id, c.holding_id, c.kind, c.title, c.url, c.login,
	(c.secret_cipher IS NOT NULL) AS has_secret,
	c.key_version, c.rotated_at, c.wiped_at, c.note, c.created_at, c.updated_at`

// CredentialView — то, что можно показывать. Секрета здесь нет по построению.
type CredentialView struct {
	model.Credential
	HasSecret bool
}

func scanCredential(row pgx.Row) (CredentialView, error) {
	var c CredentialView
	err := row.Scan(&c.ID, &c.HoldingID, &c.Kind, &c.Title, &c.URL, &c.Login,
		&c.HasSecret, &c.KeyVersion, &c.RotatedAt, &c.WipedAt, &c.Note,
		&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (r *CredentialRepository) ListByHolding(ctx context.Context, holdingID int64) ([]CredentialView, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+credentialListColumns+` FROM wh_credential c
		 WHERE c.holding_id = $1 ORDER BY c.kind, c.title`, holdingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CredentialView, 0, 4)
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *CredentialRepository) GetByID(ctx context.Context, id int64) (*CredentialView, error) {
	c, err := scanCredential(r.db.QueryRow(ctx,
		`SELECT `+credentialListColumns+` FROM wh_credential c WHERE c.id = $1`, id))
	if err != nil {
		return nil, NotFoundAs(err, apperr.CodeCredentialNotFound)
	}
	return &c, nil
}

// GetSecret — ЕДИНСТВЕННОЕ место, где шифротекст покидает базу.
// Вызывается только из сервиса reveal, только для администратора,
// и только вместе с записью в журнал просмотров.
func (r *CredentialRepository) GetSecret(ctx context.Context, id int64) (string, int16, error) {
	var cipherText *string
	var version int16
	err := r.db.QueryRow(ctx,
		`SELECT secret_cipher, key_version FROM wh_credential WHERE id = $1`, id).
		Scan(&cipherText, &version)
	if err != nil {
		return "", 0, NotFoundAs(err, apperr.CodeCredentialNotFound)
	}
	if cipherText == nil {
		// Секрет затёрт (wiped_at): строка живёт ради журнала просмотров.
		return "", 0, apperr.New(apperr.CodeCredentialWiped, "credential wiped")
	}
	return *cipherText, version, nil
}

type CredentialInput struct {
	HoldingID    int64
	Kind         string
	Title        string
	URL          *string
	Login        *string
	SecretCipher *string
	KeyVersion   int16
	Note         *string
}

func (r *CredentialRepository) Create(ctx context.Context, in CredentialInput) (*CredentialView, error) {
	var id int64
	err := r.db.QueryRow(ctx, `
		INSERT INTO wh_credential (holding_id, kind, title, url, login, secret_cipher, key_version, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id
	`, in.HoldingID, in.Kind, model.CleanText(in.Title), in.URL, in.Login,
		in.SecretCipher, in.KeyVersion, in.Note).Scan(&id)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return nil, apperr.New(apperr.CodeHoldingNotFound, "holding not found")
		}
		return nil, err
	}
	return r.GetByID(ctx, id)
}

type CredentialUpdate struct {
	Kind  *string
	Title *string
	URL   *string
	Login *string
	Note  *string
	// SecretCipher непустой — смена пароля: обновляет rotated_at,
	// и доступ уходит из отчёта «что менять после увольнения».
	SecretCipher *string
	KeyVersion   *int16
}

func (r *CredentialRepository) Update(
	ctx context.Context, id int64, upd CredentialUpdate, now time.Time,
) (*CredentialView, error) {
	q := sq.Update("wh_credential").PlaceholderFormat(sq.Dollar).Set("updated_at", sq.Expr("NOW()"))
	changed := false

	if upd.Kind != nil {
		q, changed = q.Set("kind", *upd.Kind), true
	}
	if upd.Title != nil {
		q, changed = q.Set("title", model.CleanText(*upd.Title)), true
	}
	if upd.URL != nil {
		q, changed = q.Set("url", nullIfEmpty(*upd.URL)), true
	}
	if upd.Login != nil {
		q, changed = q.Set("login", nullIfEmpty(*upd.Login)), true
	}
	if upd.Note != nil {
		q, changed = q.Set("note", nullIfEmpty(*upd.Note)), true
	}
	if upd.SecretCipher != nil {
		// Смена пароля = ротация. Без отметки rotated_at отчёт «что сменить
		// после увольнения» вечно показывал бы всё, что человек когда-либо видел.
		//
		// ⚠ wiped_at здесь БОЛЬШЕ НЕ СБРАСЫВАЕТСЯ. Затёртая запись — надгробие:
		// секрет стёрт, а журнал просмотров при ней сохранён намеренно
		// (FK на RESTRICT), чтобы не потерять след того, кто пароль видел.
		// Оживление надгробия приписывало журнал просмотров СТАРОГО пароля
		// НОВОМУ — замерено: wiped_at снимался, а запись в
		// wh_credential_view_log оставалась на месте, и аудит начинал врать.
		// Новый пароль — это НОВАЯ запись доступа, а не переиспользованное
		// надгробие.
		q = q.Set("secret_cipher", *upd.SecretCipher).
			Set("rotated_at", now)
		if upd.KeyVersion != nil {
			q = q.Set("key_version", *upd.KeyVersion)
		}
		changed = true
	}
	if !changed {
		return nil, apperr.New(apperr.CodeUpdateFieldsRequired, "update fields required")
	}

	// ⚠ Условие wiped_at IS NULL стоит в самом UPDATE, а не проверкой перед ним:
	// проверка «прочитали, потом обновили» оставляла бы окно, в которое
	// параллельное удаление успевает затереть запись.
	sqlStr, args, err := q.
		Where(sq.Eq{"id": id}).
		Where("wiped_at IS NULL").
		Suffix("RETURNING id").ToSql()
	if err != nil {
		return nil, err
	}
	var updated int64
	if err := r.db.QueryRow(ctx, sqlStr, args...).Scan(&updated); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		// Ни одной строки: либо доступа нет вовсе, либо он затёрт. Это разные
		// ответы, и различить их обязательно — «не найдено» на существующее
		// надгробие сбивает с толку.
		var exists bool
		if e := r.db.QueryRow(ctx,
			`SELECT TRUE FROM wh_credential WHERE id = $1`, id).Scan(&exists); e != nil {
			return nil, NotFoundAs(e, apperr.CodeCredentialNotFound)
		}
		return nil, apperr.New(apperr.CodeCredentialWiped,
			"доступ затёрт: его журнал просмотров относится к прежнему паролю, "+
				"поэтому запись не переиспользуется — заведите новый доступ")
	}
	return r.GetByID(ctx, id)
}

// Delete удаляет доступ, а если его уже смотрели — ЗАТИРАЕТ.
//
// Журнал просмотров стоит на RESTRICT намеренно: удаление доступа не должно
// стирать след того, кто его смотрел. Поэтому просмотренная запись не исчезает,
// а теряет секрет и получает wiped_at.
//
// ⚠ Из отчёта о ротации затёртая запись УХОДИТ, и так и задумано:
// представление wh_v_credentials_to_rotate отбирает по `c.wiped_at IS NULL`
// (migrations/00001, строка ~380). Ротировать там нечего — секрета больше нет.
// Прежняя редакция этого комментария обещала обратное («остаётся видна
// в отчёте о ротации»), что расходилось с SQL; расхождение нашло состязательное
// ревью.
//
// Журнал при надгробии сохраняется, но ЧИТАЕТСЯ он только этим представлением:
// отдельной ручки «кто смотрел пароли» в сервисе нет, и по затёртым записям
// журнал доступен только запросом в базу. Требованиям это не противоречит —
// от сервиса требуется ПИСАТЬ журнал, — но знать об этом надо.
func (r *CredentialRepository) Delete(ctx context.Context, id int64, now time.Time) (wiped bool, err error) {
	var viewed int64
	if err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM wh_credential_view_log WHERE credential_id = $1`, id).
		Scan(&viewed); err != nil {
		return false, err
	}

	if viewed > 0 {
		tag, err := r.db.Exec(ctx, `
			UPDATE wh_credential
			SET secret_cipher = NULL, wiped_at = $2, updated_at = NOW()
			WHERE id = $1 AND wiped_at IS NULL
		`, id, now)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 {
			// уже затёрт — операция идемпотентна
			if _, err := r.GetByID(ctx, id); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	tag, err := r.db.Exec(ctx, `DELETE FROM wh_credential WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, apperr.New(apperr.CodeCredentialNotFound, "credential not found")
	}
	return false, nil
}

// LogView записывает просмотр пароля.
//
// Это единственный способ ответить на вопросы «кто видел пароль от коммутатора
// до того, как сеть легла» и «что менять после увольнения».
func (r *CredentialRepository) LogView(
	ctx context.Context, credentialID, userID int64, ip *string, now time.Time,
) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO wh_credential_view_log (credential_id, user_id, viewed_at, ip_address)
		VALUES ($1, $2, $3, $4)
	`, credentialID, userID, now, ip)
	return err
}

// ToRotate — что менять после увольнения: доступы, которые человек видел
// и которые с тех пор не меняли. Если userID нил — по всем пользователям.
func (r *CredentialRepository) ToRotate(ctx context.Context, userID *int64) ([]model.CredentialToRotateRow, error) {
	q := `SELECT user_id, credential_id, kind, title, device_name, inv_number, warehouse,
	             last_seen_at, rotated_at
	      FROM wh_v_credentials_to_rotate`
	args := []any{}
	if userID != nil {
		q += ` WHERE user_id = $1`
		args = append(args, *userID)
	}
	q += ` ORDER BY last_seen_at DESC`

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.CredentialToRotateRow, 0, 16)
	for rows.Next() {
		var c model.CredentialToRotateRow
		if err := rows.Scan(&c.UserID, &c.CredentialID, &c.Kind, &c.Title, &c.DeviceName,
			&c.InvNumber, &c.Warehouse, &c.LastSeenAt, &c.RotatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
