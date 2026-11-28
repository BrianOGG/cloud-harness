-- One index entry per live message. Replacing an entry advances its revision,
-- even when a late message sorts before the user's last chronological page.
CREATE TABLE message_sync (
  revision INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  message_id TEXT NOT NULL,
  UNIQUE(session_id, message_id)
);
CREATE INDEX idx_message_sync_revision ON message_sync(session_id, revision);
INSERT INTO message_sync(session_id, message_id)
SELECT session_id, id FROM messages ORDER BY session_id, created_at, id;

CREATE TRIGGER message_sync_insert AFTER INSERT ON messages BEGIN
  INSERT INTO message_sync(session_id, message_id) VALUES(NEW.session_id, NEW.id);
END;
CREATE TRIGGER message_sync_update AFTER UPDATE ON messages BEGIN
  DELETE FROM message_sync WHERE session_id = OLD.session_id AND message_id = OLD.id;
  INSERT INTO message_sync(session_id, message_id) VALUES(NEW.session_id, NEW.id);
END;
-- Retention drops both the content and its index entry. One marker per
-- session tells stale readers to replace their cache without keeping tombstones.
CREATE TRIGGER message_sync_delete AFTER DELETE ON messages
WHEN EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id) BEGIN
  DELETE FROM message_sync WHERE session_id = OLD.session_id AND message_id IN (OLD.id, '');
  INSERT INTO message_sync(session_id, message_id) VALUES(OLD.session_id, '');
END;
