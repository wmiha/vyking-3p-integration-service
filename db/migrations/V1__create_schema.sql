-- Schema as provided by the task. It is owned by another team and treated as
-- fixed: these migrations exist for local development and tests only and must
-- never be run against production (see NOTES.md).
-- The only change is that comments wrapped across lines in the source document
-- were joined so the script is valid SQL.

CREATE TABLE brands (
    BRD_iID   INT UNSIGNED NOT NULL PRIMARY KEY,
    BRD_cName VARCHAR(64)  NOT NULL
);

CREATE TABLE customers (
    CUS_iID        INT UNSIGNED NOT NULL PRIMARY KEY,
    CUS_BRD_iID    INT UNSIGNED NOT NULL,           -- which brand
    CUS_cUserName  VARCHAR(64)  NULL,
    CUS_cEmail     VARCHAR(190) NULL,
    CUS_cFirstName VARCHAR(64)  NULL,
    CUS_cLastName  VARCHAR(64)  NULL,
    CUS_cPhone     VARCHAR(32)  NULL,
    CUS_cCountry   CHAR(2)      NULL,
    CUS_CUR_iID    INT UNSIGNED NOT NULL,           -- the customer's currency
    CUS_dCreated   DATETIME     NOT NULL,
    CUS_bActive    TINYINT(1)   NOT NULL DEFAULT 1,
    UNIQUE KEY (CUS_BRD_iID, CUS_cEmail),            -- unique per brand, not across brands
    KEY (CUS_cEmail),
    KEY (CUS_BRD_iID)
);

CREATE TABLE currencies (
    CUR_iID         INT UNSIGNED     NOT NULL PRIMARY KEY,
    CUR_cAlpha3     CHAR(3)          NOT NULL,      -- e.g. EUR, JPY, BHD
    CUR_iMinorUnits TINYINT UNSIGNED NULL           -- 2 for EUR, 0 for JPY, 3 for BHD
);

CREATE TABLE transactions (
    TRX_iID      BIGINT UNSIGNED NOT NULL PRIMARY KEY,
    TRX_CUS_iID  INT UNSIGNED    NOT NULL,
    TRX_TYP_iID  INT UNSIGNED    NOT NULL,           -- see transaction_types
    TRX_STA_iID  INT UNSIGNED    NOT NULL,           -- see transaction_statuses
    TRX_iAmount  BIGINT          NULL,               -- MINOR UNITS, nullable
    TRX_PMT_iID  INT UNSIGNED    NULL,               -- see payment_methods
    TRX_dCreated DATETIME        NOT NULL,
    TRX_dBooked  DATETIME        NULL,               -- when it was accepted for processing
    TRX_dChanged DATETIME        NOT NULL,           -- last state change
    KEY (TRX_CUS_iID, TRX_dCreated)
);

CREATE TABLE transaction_types (
    TYP_iID   INT UNSIGNED NOT NULL PRIMARY KEY,
    TYP_cName VARCHAR(64)  NOT NULL
);

CREATE TABLE transaction_statuses (
    STA_iID   INT UNSIGNED NOT NULL PRIMARY KEY,
    STA_cName VARCHAR(64)  NOT NULL
);

CREATE TABLE payment_methods (
    PMT_iID   INT UNSIGNED NOT NULL PRIMARY KEY,
    PMT_cName VARCHAR(64)  NOT NULL                 -- e.g. 'PAYIN CARD'
);

CREATE TABLE sync_queue (
    SYQ_iID        BIGINT UNSIGNED NOT NULL PRIMARY KEY,
    SYQ_CUS_iID    INT UNSIGNED    NOT NULL,
    SYQ_dQueued    DATETIME        NOT NULL,
    SYQ_dProcessed DATETIME        NULL              -- NULL means outstanding
);

CREATE TABLE partner_contacts (
    PCO_CUS_iID    INT UNSIGNED NOT NULL PRIMARY KEY,
    PCO_cContactId VARCHAR(64)  NOT NULL,            -- the id Acme Support gave us
    PCO_dSynced    DATETIME     NOT NULL
);
