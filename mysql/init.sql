CREATE TABLE IF NOT EXISTS settings (
    id INT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(64) NOT NULL UNIQUE,
    value VARCHAR(255) NOT NULL
);

INSERT INTO settings (name, value) VALUES
    ('site_name', 'MySQL Proxy Demo'),
    ('version', '1.0');