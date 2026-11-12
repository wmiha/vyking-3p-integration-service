-- Reference data as provided by the task.

INSERT INTO transaction_types (TYP_iID, TYP_cName) VALUES
    (1001, 'PAYIN'),
    (2001, 'PAYOUT'),
    (3001, 'ADJUSTMENT');

INSERT INTO transaction_statuses (STA_iID, STA_cName) VALUES
    (10, 'reserved'),
    (20, 'pending_provider'),
    (30, 'awaiting_approval'),
    (40, 'approved'),
    (50, 'booked'),
    (60, 'settled'),
    (61, 'settled_late'),
    (70, 'declined_provider'),
    (71, 'declined_risk'),
    (72, 'declined_limit'),
    (80, 'cancelled_by_customer'),
    (81, 'cancelled_by_operator'),
    (82, 'cancelled_timeout'),
    (90, 'reversed'),
    (91, 'chargeback');

INSERT INTO currencies (CUR_iID, CUR_cAlpha3, CUR_iMinorUnits) VALUES
    (1, 'EUR', 2),
    (2, 'GBP', 2),
    (3, 'JPY', 0),
    (4, 'BHD', 3),
    (5, 'USD', NULL);
