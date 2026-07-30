// Package crypto шифрует пароли от техники.
//
// База с инвентаризацией — неприятная утечка. База с паролями от роутеров
// и серверов — компрометация инфраструктуры. Отсюда три требования, которые
// не подлежат упрощению:
//
//  1. AES-256-GCM, ключ 32 байта ТОЛЬКО в переменной окружения.
//     В базе ключа быть не должно: иначе бэкап содержит и замок, и ключ.
//     ⚠ Потеря ключа = безвозвратная потеря всех паролей, расшифровать нечем.
//  2. Секрет не отдаётся в списках — только отдельным вызовом reveal.
//  3. Просмотр пишется в журнал.
//
// Пункты 2 и 3 живут в service/handler; здесь — только сам шифр.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CurrentKeyVersion — версия ключа, которой шифруются новые записи.
//
// ⚠ РОТАЦИЯ ТРЕБУЕТ ПЕРЕСБОРКИ, А НЕ ПРАВКИ .env. Это константа: New всегда
// привязывает пришедший ключ именно к ней, Encrypt всегда пишет в конверт
// именно её. Значит ВСЕ сохранённые записи помечены v1, и после простой замены
// WAREHOUSE_CREDENTIALS_KEY новый ключ занимает v1, а RegisterOldKey со старым
// отвергается как «версия занята текущим ключом» — читать прежние записи
// становится нечем, и отказ выглядит как порча данных.
//
// Порядок ротации: поднять эту константу, пересобрать, зарегистрировать старый
// ключ под прежним номером. Чтения старых ключей из окружения в app.go
// и config.go ПОКА НЕТ, поэтому RegisterOldKey вызывается только из тестов.
// До появления этого плюмбинга смена ключа равносильна его потере.
const CurrentKeyVersion int16 = 1

const (
	nonceSize = 12 // стандарт для GCM
	tagSize   = 16
	keySize   = 32 // AES-256
)

var (
	ErrKeyMissing  = errors.New("ключ шифрования не задан")
	ErrKeySize     = errors.New("ключ шифрования должен быть ровно 32 байта в base64")
	ErrBadEnvelope = errors.New("повреждённый конверт секрета")
	ErrUnknownKey  = errors.New("нет ключа для версии конверта")
)

// Cipher шифрует и расшифровывает секреты.
type Cipher struct {
	// aeads по версии ключа: текущая — для записи, остальные — только чтение.
	aeads map[int16]cipher.AEAD
}

// New создаёт шифр из base64-ключа (переменная WAREHOUSE_CREDENTIALS_KEY).
func New(keyBase64 string) (*Cipher, error) {
	aead, err := aeadFromBase64(keyBase64)
	if err != nil {
		return nil, err
	}
	return &Cipher{aeads: map[int16]cipher.AEAD{CurrentKeyVersion: aead}}, nil
}

// RegisterOldKey добавляет ключ предыдущей версии — нужен только для чтения
// записей, зашифрованных до ротации.
func (c *Cipher) RegisterOldKey(version int16, keyBase64 string) error {
	if version == CurrentKeyVersion {
		return fmt.Errorf("версия %d занята текущим ключом", version)
	}
	aead, err := aeadFromBase64(keyBase64)
	if err != nil {
		return err
	}
	c.aeads[version] = aead
	return nil
}

func aeadFromBase64(keyBase64 string) (cipher.AEAD, error) {
	keyBase64 = strings.TrimSpace(keyBase64)
	if keyBase64 == "" {
		return nil, ErrKeyMissing
	}
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		// В ошибку НЕ подставляем сам ключ — она уйдёт в лог.
		return nil, fmt.Errorf("%w: не удалось декодировать base64", ErrKeySize)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("%w: получено %d байт", ErrKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt возвращает конверт вида "v{версия}:base64(nonce ‖ tag ‖ cipher)".
//
// ⚠ ПОРЯДОК ЧАСТЕЙ. Go отдаёт из Seal «cipher ‖ tag», а формат конверта
// требует «nonce ‖ tag ‖ cipher» — тег ВПЕРЕДИ, как его кладёт PHP-реализация
// (symfony_documents_flow/src/Service/SpaApi/Inventory/CredentialCipher.php).
// Порядок фиксирует TestEnvelopeLayout, менять его нельзя.
//
// ⚠ НО ОБМЕНА КОНВЕРТАМИ С МОНОЛИТОМ НЕТ, и путать одно с другим опасно.
// Это два независимых модуля: таблицы разные (wh_credential против
// inventory_device_credential), базы разные (5434 против 5432), ключи разные
// (WAREHOUSE_ против INVENTORY_CREDENTIALS_KEY), а межсервисный клиент умеет
// ровно две ручки — справочники пользователей и подразделений. Единый порядок
// частей выбран ради единообразия для человека, читающего обе кодовые базы,
// а не как контракт передачи данных.
//
// Отсюда следствие, из-за которого этот абзац и написан: ВНУТРИ конверта
// у нас лежит только пароль (логин — отдельная колонка login), а у монолита —
// JSON {login, password}, потому что у его сущности колонки login нет вовсе.
// Раскладки конвертов совместимы, НАГРУЗКИ — нет. Приводить нагрузку
// «ради совместимости» НЕЛЬЗЯ: все уже сохранённые пароли станут нечитаемы,
// причём молча — конверт продолжит расшифровываться, а разбор нагрузки упадёт.
// Ревью 29.07.2026 предлагало именно такую починку; она хуже бездействия.
func (c *Cipher) Encrypt(plaintext string) (string, int16, error) {
	aead, ok := c.aeads[CurrentKeyVersion]
	if !ok {
		return "", 0, ErrKeyMissing
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", 0, fmt.Errorf("генерация nonce: %w", err)
	}

	sealed := aead.Seal(nil, nonce, []byte(plaintext), nil)
	if len(sealed) < tagSize {
		return "", 0, ErrBadEnvelope
	}
	body, tag := sealed[:len(sealed)-tagSize], sealed[len(sealed)-tagSize:]

	envelope := make([]byte, 0, len(nonce)+len(tag)+len(body))
	envelope = append(envelope, nonce...)
	envelope = append(envelope, tag...)
	envelope = append(envelope, body...)

	return fmt.Sprintf("v%d:%s", CurrentKeyVersion,
		base64.StdEncoding.EncodeToString(envelope)), CurrentKeyVersion, nil
}

// Decrypt разбирает конверт и возвращает секрет.
func (c *Cipher) Decrypt(envelope string) (string, error) {
	colon := strings.IndexByte(envelope, ':')
	if colon < 2 || envelope[0] != 'v' {
		return "", ErrBadEnvelope
	}
	// ⚠ Ширина типа задаётся ЯВНО. strconv.Atoi возвращает int, и приведение
	// int16(version) МОЛЧА усекало старшие биты: «v65537:», «v131073:»
	// и «v-65535:» превращались в v1, и конверт ПРИНИМАЛСЯ — проверено запуском.
	// PHP-реализация такие версии отвергает регекспом ^v(\d+):, то есть Go был
	// снисходительнее к недоверенному вводу, чем монолит.
	//
	// ParseUint отсекает и знак, и переполнение. Отдельная проверка MaxInt16
	// нужна потому, что bitSize=16 допускает до 65535, а версии у нас int16.
	version64, err := strconv.ParseUint(envelope[1:colon], 10, 16)
	if err != nil || version64 > math.MaxInt16 {
		return "", ErrBadEnvelope
	}
	version := int16(version64)
	aead, ok := c.aeads[version]
	if !ok {
		return "", fmt.Errorf("%w: v%d", ErrUnknownKey, version)
	}

	raw, err := base64.StdEncoding.DecodeString(envelope[colon+1:])
	if err != nil {
		return "", ErrBadEnvelope
	}
	// Строго «меньше»: у пустого секрета тело конверта равно ровно nonce+tag,
	// и это законный конверт, а не повреждённый.
	if len(raw) < nonceSize+tagSize {
		return "", ErrBadEnvelope
	}

	nonce := raw[:nonceSize]
	tag := raw[nonceSize : nonceSize+tagSize]
	body := raw[nonceSize+tagSize:]

	// обратно в порядок, которого ждёт Go: cipher ‖ tag
	sealed := make([]byte, 0, len(body)+len(tag))
	sealed = append(sealed, body...)
	sealed = append(sealed, tag...)

	plain, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		// Текст не раскрывает, ЧЕМ ИМЕННО конверт плох, но обе возможные
		// причины называет. Неверный ключ и испорченные байты дают ОДИН И ТОТ ЖЕ
		// отказ GCM — различить их нельзя даже теоретически (проверено: тексты
		// ошибок совпадали до символа). Молчать об этом дорого: «повреждённый
		// конверт» читается как порча базы, рядом в README стоит «потеря ключа
		// необратима», и напрашивается вывод «пароли потеряны» — тогда как
		// достаточно вернуть прежний WAREHOUSE_CREDENTIALS_KEY.
		//
		// Наружу этот текст не уходит: helper.WriteError отдаёт клиенту только
		// код ошибки, сообщение остаётся в логе, где нет ни ключа, ни конверта.
		return "", fmt.Errorf(
			"%w: не прошла проверка подлинности — неверный ключ ИЛИ повреждённые данные",
			ErrBadEnvelope)
	}
	return string(plain), nil
}

// GenerateKey создаёт новый ключ в base64 — для первичной настройки окружения.
func GenerateKey() (string, error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}
