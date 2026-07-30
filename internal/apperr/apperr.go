// Package apperr определяет доменные ошибки, независимые от транспорта (HTTP).
//
// Слои repository и service возвращают эти ошибки (через errors.Is / оборачивание
// в %w), а слой handler мапит их в HTTP-статусы функцией helper.WriteError.
// Так бизнес-логика не знает про HTTP, а handler не угадывает статус по тексту.
package apperr

type ErrorCode string

const (
	CodeNotFound     ErrorCode = "not_found"
	CodeForbidden    ErrorCode = "forbidden"
	CodeUnauthorized ErrorCode = "unauthorized"
	CodeConflict     ErrorCode = "conflict"
	CodeValidation   ErrorCode = "validation_failed"

	CodeItemNotFound       ErrorCode = "item_not_found"
	CodeHoldingNotFound    ErrorCode = "holding_not_found"
	CodeWarehouseNotFound  ErrorCode = "warehouse_not_found"
	CodeCredentialNotFound ErrorCode = "credential_not_found"

	CodeInvalidJSON          ErrorCode = "invalid_json"
	CodeUpdateFieldsRequired ErrorCode = "update_fields_required"
	CodeInsufficientRights   ErrorCode = "insufficient_rights"

	CodeItemNameRequired      ErrorCode = "item_name_required"
	CodeInvalidArea           ErrorCode = "invalid_area"
	CodeInvalidHolderType     ErrorCode = "invalid_holder_type"
	CodeInvalidOutcome        ErrorCode = "invalid_outcome"
	CodeInvalidResolution     ErrorCode = "invalid_resolution"
	CodeInvalidQuantity       ErrorCode = "invalid_quantity"
	CodeHolderShapeInvalid    ErrorCode = "holder_shape_invalid"
	CodeInvNumberQtyInvalid   ErrorCode = "inv_number_qty_invalid"
	CodeInvNumberDuplicate    ErrorCode = "inv_number_duplicate"
	CodeWarehouseNameRequired ErrorCode = "warehouse_name_required"
	CodeWarehouseNameTaken    ErrorCode = "warehouse_name_taken"
	CodeWarehouseHasHoldings  ErrorCode = "warehouse_has_holdings"
	CodeItemHasHoldings       ErrorCode = "item_has_holdings"

	// Импорт
	CodeFileNotProvided  ErrorCode = "file_not_provided"
	CodeUnknownFormat    ErrorCode = "unknown_import_format"
	CodeImportFailed     ErrorCode = "import_failed"
	CodeFileTooLarge     ErrorCode = "file_too_large"
	CodeSheetLayoutUnkno ErrorCode = "sheet_layout_unknown"

	// Доступы к технике
	CodeCredentialsKeyMissing ErrorCode = "credentials_key_missing"
	CodeCredentialWiped       ErrorCode = "credential_wiped"
	CodeCredentialKindInvalid ErrorCode = "credential_kind_invalid"
	CodeCredentialViewed      ErrorCode = "credential_viewed"
	CodeSecretRequired        ErrorCode = "secret_required"
)

type Error struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.Err
}

func (e *Error) Is(target error) bool {
	targetErr, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Code == targetErr.Code || categoryCode(e.Code) == targetErr.Code
}

func New(code ErrorCode, msg string) *Error {
	return &Error{
		Code:    code,
		Message: msg,
	}
}

// Wrap сохраняет исходную ошибку для лога, наружу отдаётся только код.
func Wrap(code ErrorCode, msg string, err error) *Error {
	return &Error{Code: code, Message: msg, Err: err}
}

func categoryCode(code ErrorCode) ErrorCode {
	switch code {
	case CodeItemNotFound,
		CodeHoldingNotFound,
		CodeWarehouseNotFound,
		CodeCredentialNotFound:
		return CodeNotFound
	case CodeInsufficientRights:
		return CodeForbidden
	case CodeInvNumberDuplicate,
		CodeWarehouseNameTaken,
		CodeWarehouseHasHoldings,
		CodeItemHasHoldings,
		CodeCredentialViewed:
		return CodeConflict
	case CodeInvalidJSON,
		CodeUpdateFieldsRequired,
		CodeItemNameRequired,
		CodeInvalidArea,
		CodeInvalidHolderType,
		CodeInvalidOutcome,
		CodeInvalidResolution,
		CodeInvalidQuantity,
		CodeHolderShapeInvalid,
		CodeInvNumberQtyInvalid,
		CodeWarehouseNameRequired,
		CodeFileNotProvided,
		CodeUnknownFormat,
		CodeFileTooLarge,
		CodeCredentialKindInvalid,
		CodeCredentialWiped,
		CodeSecretRequired:
		return CodeValidation
	default:
		return code
	}
}

var (
	// ErrNotFound — запрошенная сущность не существует. → 404
	ErrNotFound = New(CodeNotFound, "not found")

	// ErrForbidden — пользователь аутентифицирован, но не имеет прав на ресурс. → 403
	ErrForbidden = New(CodeForbidden, "forbidden")

	// ErrUnauthorized — пользователь не аутентифицирован. → 401
	ErrUnauthorized = New(CodeUnauthorized, "unauthorized")

	// ErrConflict — нарушение инварианта/уникальности (дубликат и т.п.). → 409
	ErrConflict = New(CodeConflict, "conflict")

	// ErrValidation — некорректные входные данные запроса. → 400
	ErrValidation = New(CodeValidation, "validation failed")
)
