package main

// Snapshot a room's readership once per broadcast. Public traffic doesn't need
// a separate SQLite lookup per recipient, while private traffic stays scoped.
func (s *Store) Readers(room string) (bool, map[string]bool) {
	r, e := s.RoomInfo(room)
	allowed := map[string]bool{}
	if e != nil {
		return false, allowed
	}
	if !r.Private {
		return true, nil
	}
	allowed[r.Owner] = true
	rows, e := s.readDB().Query(`SELECT user FROM members WHERE room=?`, room)
	if e != nil {
		return false, map[string]bool{}
	}
	defer rows.Close()
	for rows.Next() {
		var user string
		if rows.Scan(&user) == nil {
			allowed[user] = true
		}
	}
	return false, allowed
}
