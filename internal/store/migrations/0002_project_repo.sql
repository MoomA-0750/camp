-- cwd はリポジトリのルートとは限らない。実測35個の cwd のうち、
-- unibridge/node_modules/... のような「作業中に cd しただけ」が大半で、
-- リポジトリのルートは数個しかない。解決結果を持つ列を足す。
ALTER TABLE projects ADD COLUMN is_repo INTEGER NOT NULL DEFAULT 0;
