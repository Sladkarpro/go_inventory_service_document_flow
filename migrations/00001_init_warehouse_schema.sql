-- +goose Up

CREATE TABLE users (
    id         BIGINT PRIMARY KEY,
    login      VARCHAR(50)  NOT NULL,
    lastname   VARCHAR(50)  NOT NULL,
    firstname  VARCHAR(50)  NOT NULL,
    patronymic VARCHAR(50),
    deleted_at TIMESTAMPTZ(0),
    synced_at  TIMESTAMPTZ(0) NOT NULL DEFAULT NOW()
);
CREATE INDEX ix_users_fio ON users (
    LOWER(TRIM(lastname || ' ' || firstname || ' ' || COALESCE(patronymic, '')))
);

CREATE TABLE departments (
    id         BIGINT PRIMARY KEY,
    name       VARCHAR(255) NOT NULL,
    parent_id  BIGINT,
    deleted_at TIMESTAMPTZ(0),
    synced_at  TIMESTAMPTZ(0) NOT NULL DEFAULT NOW()
);

CREATE TABLE wh_warehouse (
    id                  BIGSERIAL PRIMARY KEY,
    name                VARCHAR(255) NOT NULL,
    department_id       BIGINT,
    department_name     VARCHAR(255),
    responsible_user_id BIGINT,
    location            VARCHAR(255),
    is_active           BOOLEAN NOT NULL DEFAULT TRUE,
    note                TEXT,
    created_at          TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_wh_warehouse_name UNIQUE (name)
);
CREATE INDEX ix_wh_warehouse_responsible ON wh_warehouse (responsible_user_id);

CREATE TABLE wh_item (
    id         BIGSERIAL PRIMARY KEY,
    name       VARCHAR(500) NOT NULL,
    name_norm  VARCHAR(500) NOT NULL,
    area       VARCHAR(20),
    unit       VARCHAR(20),
    attributes JSONB NOT NULL DEFAULT '{}'::jsonb,
    note       TEXT,
    created_at TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_wh_item_area CHECK (area IS NULL OR area IN ('it', 'procurement', 'facility')),
    CONSTRAINT uq_wh_item_name_norm UNIQUE (name_norm)
);
CREATE INDEX ix_wh_item_area ON wh_item (area);

CREATE TABLE wh_holding (
    id              BIGSERIAL PRIMARY KEY,
    item_id         BIGINT NOT NULL REFERENCES wh_item (id) ON DELETE RESTRICT,
    holder_type     VARCHAR(10) NOT NULL,
    
    user_id         BIGINT,
    user_fio        VARCHAR(255),
    department_id   BIGINT,
    department_name VARCHAR(255),
    warehouse_id    BIGINT REFERENCES wh_warehouse (id) ON DELETE RESTRICT,
    room            VARCHAR(255),

    inv_number      VARCHAR(50),
    inv_number_norm VARCHAR(50),
    serial_number   VARCHAR(100),

    hostname        VARCHAR(255),
    ip_address      INET,
    mac_address     VARCHAR(17),
    specs           JSONB NOT NULL DEFAULT '{}'::jsonb,

    qty_expected    NUMERIC(14,3) NOT NULL,
    qty_actual      NUMERIC(14,3),
    qty_split       NUMERIC(14,3) NOT NULL DEFAULT 0,
    outcome         VARCHAR(20),
    counted_at      TIMESTAMPTZ(0),
    counted_by_id   BIGINT,
    note            TEXT,

    resolution      VARCHAR(20),
    resolution_ref  VARCHAR(255),
    resolved_at     TIMESTAMPTZ(0),
    resolved_by_id  BIGINT,

    source_file     VARCHAR(255),
    source_sheet    VARCHAR(255),
    source_row      INT,
    created_at      TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_wh_holding_type CHECK (holder_type IN ('person','department','warehouse','unknown')),
    CONSTRAINT chk_wh_holding_outcome CHECK (outcome IS NULL OR outcome IN ('found','missing','broken','in_repair','surplus')),
    CONSTRAINT chk_wh_holding_resolution CHECK (resolution IS NULL OR resolution IN ('written_off','found_later','explained','pending')),
    CONSTRAINT chk_wh_holding_qty CHECK (qty_expected >= 0 AND (qty_actual IS NULL OR qty_actual >= 0)),
    CONSTRAINT chk_wh_holding_qty_split CHECK (qty_split >= 0),
    CONSTRAINT chk_wh_holding_shape CHECK (
        (holder_type = 'person'     AND (user_id IS NOT NULL OR user_fio IS NOT NULL)) OR
        (holder_type = 'department' AND (department_id IS NOT NULL OR department_name IS NOT NULL) AND user_id IS NULL AND user_fio IS NULL) OR
        (holder_type = 'warehouse'  AND warehouse_id IS NOT NULL AND user_id IS NULL AND user_fio IS NULL) OR
        (holder_type = 'unknown'    AND user_id IS NULL AND user_fio IS NULL AND department_id IS NULL AND department_name IS NULL)
    ),
    CONSTRAINT chk_wh_holding_counted CHECK (outcome IS NULL OR outcome = 'in_repair' OR qty_actual IS NOT NULL),
    CONSTRAINT chk_wh_holding_inv_qty CHECK (inv_number IS NULL OR qty_expected IN (0, 1)),
    CONSTRAINT chk_wh_holding_inv_norm CHECK (
        (inv_number IS NULL AND inv_number_norm IS NULL) OR 
        (inv_number IS NOT NULL AND inv_number_norm IS NOT NULL)
    )
);

CREATE UNIQUE INDEX uq_wh_holding_inv_number ON wh_holding (inv_number_norm) WHERE inv_number_norm IS NOT NULL;
CREATE UNIQUE INDEX uq_wh_holding_natural ON wh_holding (
    item_id, holder_type, COALESCE(user_id, 0), COALESCE(user_fio, ''),
    COALESCE(department_name, ''), COALESCE(warehouse_id, 0), COALESCE(room, '')
) WHERE inv_number_norm IS NULL;

CREATE INDEX ix_wh_holding_serial  ON wh_holding (serial_number) WHERE serial_number IS NOT NULL;
CREATE INDEX ix_wh_holding_item    ON wh_holding (item_id);
CREATE INDEX ix_wh_holding_user    ON wh_holding (user_id)       WHERE holder_type = 'person';
CREATE INDEX ix_wh_holding_dept    ON wh_holding (department_id) WHERE holder_type = 'department';
CREATE INDEX ix_wh_holding_wh      ON wh_holding (warehouse_id)  WHERE holder_type = 'warehouse';
CREATE INDEX ix_wh_holding_type    ON wh_holding (holder_type, outcome);
CREATE INDEX ix_wh_holding_uncount ON wh_holding (item_id)       WHERE qty_actual IS NULL;

CREATE TABLE wh_credential (
    id            BIGSERIAL PRIMARY KEY,
    holding_id    BIGINT NOT NULL REFERENCES wh_holding (id) ON DELETE CASCADE,
    kind          VARCHAR(20)  NOT NULL,
    title         VARCHAR(255) NOT NULL,
    url           VARCHAR(500),
    login         VARCHAR(255),
    secret_cipher TEXT,
    key_version   SMALLINT NOT NULL DEFAULT 1,
    rotated_at    TIMESTAMPTZ(0),
    wiped_at      TIMESTAMPTZ(0),
    note          TEXT,
    created_at    TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_wh_credential_kind CHECK (kind IN ('admin','ssh','wifi','rdp','other')),
    CONSTRAINT chk_wh_credential_wiped CHECK (
        (wiped_at IS NULL AND secret_cipher IS NOT NULL) OR 
        (wiped_at IS NOT NULL AND secret_cipher IS NULL)
    )
);
CREATE INDEX ix_wh_credential_holding ON wh_credential (holding_id);

CREATE TABLE wh_credential_view_log (
    id            BIGSERIAL PRIMARY KEY,
    credential_id BIGINT NOT NULL REFERENCES wh_credential (id) ON DELETE RESTRICT,
    user_id       BIGINT NOT NULL,
    viewed_at     TIMESTAMPTZ(0) NOT NULL DEFAULT NOW(),
    ip_address    INET
);
CREATE INDEX ix_wh_cred_view_user ON wh_credential_view_log (user_id, viewed_at);
CREATE INDEX ix_wh_cred_view_cred ON wh_credential_view_log (credential_id, viewed_at);

CREATE VIEW wh_v_losses AS
SELECT i.id AS item_id, i.name, i.area,
       h.id AS holding_id, h.holder_type,
       COALESCE(u.lastname || ' ' || u.firstname, h.user_fio)  AS holder_person,
       COALESCE(d.name, h.department_name)                     AS holder_department,
       w.name AS warehouse,
       COALESCE(mu.lastname || ' ' || mu.firstname, '')        AS responsible_person,
       w.responsible_user_id,
       h.room, h.inv_number, h.serial_number,
       h.qty_expected, h.qty_actual,
       h.qty_actual - h.qty_expected AS qty_diff,
       CASE
           WHEN h.outcome = 'broken'    THEN 'поломка'
           WHEN h.outcome = 'in_repair' THEN 'в ремонте'
           WHEN h.qty_actual < h.qty_expected THEN 'недостача'
           WHEN h.qty_actual > h.qty_expected THEN 'излишек'
           ELSE 'прочее'
       END AS discrepancy_kind,
       h.outcome, h.resolution, h.resolution_ref, h.note
FROM wh_holding h
JOIN wh_item i           ON i.id = h.item_id
LEFT JOIN users u        ON u.id = h.user_id
LEFT JOIN departments d  ON d.id = h.department_id
LEFT JOIN wh_warehouse w ON w.id = h.warehouse_id
LEFT JOIN users mu       ON mu.id = w.responsible_user_id
WHERE h.qty_actual IS NOT NULL
  AND (h.qty_actual <> h.qty_expected OR h.outcome IN ('broken', 'in_repair'));

CREATE VIEW wh_v_item_totals AS
SELECT i.id AS item_id, i.name, i.area, i.unit,
       SUM(h.qty_expected) FILTER (WHERE h.holder_type='person')     AS person_expected,
       SUM(h.qty_expected) FILTER (WHERE h.holder_type='department') AS department_expected,
       SUM(h.qty_expected) FILTER (WHERE h.holder_type='warehouse')  AS unallocated_expected,
       SUM(h.qty_expected)                                           AS total_expected,
       SUM(h.qty_actual)                                             AS total_actual,
       COUNT(*) FILTER (WHERE h.qty_actual IS NULL)                  AS not_counted_rows
FROM wh_item i
LEFT JOIN wh_holding h ON h.item_id = i.id
GROUP BY i.id, i.name, i.area, i.unit;

CREATE VIEW wh_v_credentials_to_rotate AS
SELECT v.user_id,
       c.id AS credential_id, c.kind, c.title,
       i.name AS device_name, h.inv_number, w.name AS warehouse,
       MAX(v.viewed_at) AS last_seen_at,
       c.rotated_at
FROM wh_credential_view_log v
JOIN wh_credential c     ON c.id = v.credential_id
JOIN wh_holding    h     ON h.id = c.holding_id
JOIN wh_item       i     ON i.id = h.item_id
LEFT JOIN wh_warehouse w ON w.id = h.warehouse_id
WHERE c.wiped_at IS NULL
  AND v.viewed_at > COALESCE(c.rotated_at, '-infinity'::timestamptz)
GROUP BY v.user_id, c.id, c.kind, c.title, i.name, h.inv_number, w.name, c.rotated_at;

-- +goose Down
DROP VIEW IF EXISTS wh_v_credentials_to_rotate;
DROP VIEW IF EXISTS wh_v_item_totals;
DROP VIEW IF EXISTS wh_v_losses;
DROP TABLE IF EXISTS wh_credential_view_log;
DROP TABLE IF EXISTS wh_credential;
DROP TABLE IF EXISTS wh_holding;
DROP TABLE IF EXISTS wh_item;
DROP TABLE IF EXISTS wh_warehouse;
DROP TABLE IF EXISTS departments;
DROP TABLE IF EXISTS users;
