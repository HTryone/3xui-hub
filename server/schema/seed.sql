-- ControlHub 默认管理员（admin / admin123）
INSERT INTO admins (username, password, google2fa_enabled, created_at, updated_at)
VALUES ('admin', '$2y$12$SpzsSTwyRz4OdI6afZLnS.yfrlG55y4PFJyQG5WbYbizjXQKJTnqO', 0, NOW(), NOW())
ON DUPLICATE KEY UPDATE username = username;
