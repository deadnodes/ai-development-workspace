CREATE VIRTUAL TABLE code_index_search USING fts5(words, content='code_index_cards', tokenize='unicode61 remove_diacritics 0');
CREATE TRIGGER code_index_card_insert AFTER INSERT ON code_index_cards BEGIN
 INSERT INTO code_index_search(rowid,words) VALUES(NEW.rowid,NEW.words);
END;
CREATE TRIGGER code_index_card_delete AFTER DELETE ON code_index_cards BEGIN
 INSERT INTO code_index_search(code_index_search,rowid,words) VALUES('delete',OLD.rowid,OLD.words);
END;
INSERT INTO code_index_search(code_index_search) VALUES('rebuild');
