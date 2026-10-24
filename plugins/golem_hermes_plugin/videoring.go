package main

import (
	"time"
)

// ============================================================
// 视频素材环：与图片环同构——只存群内视频的元数据（md5/aeskey/size/duration），
// 被二创点单才经宿主 DownloadVideo 拉字节。TTL 与容量同图片环。
// ============================================================

type ringVideo struct {
	MsgID     int64
	Chat      string
	Wxid      string
	Name      string
	Md5       string
	AesKey    string
	Size      uint32
	Duration  uint32
	CreatedAt int64
}

func (s *store) initVideoRing() {
	if s == nil || s.db == nil {
		return
	}
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS video_ring(
		msg_id INTEGER PRIMARY KEY, chat TEXT, wxid TEXT DEFAULT '', name TEXT DEFAULT '',
		md5 TEXT DEFAULT '', aeskey TEXT DEFAULT '', size INTEGER DEFAULT 0, duration INTEGER DEFAULT 0, created_at INTEGER)`)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_vring_chat ON video_ring(chat, created_at)`)
}

func (s *store) ringAddVideo(v ringVideo) {
	if s == nil || s.db == nil || v.Chat == "" || v.Md5 == "" {
		return
	}
	v.CreatedAt = time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT OR REPLACE INTO video_ring(msg_id,chat,wxid,name,md5,aeskey,size,duration,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		v.MsgID, v.Chat, v.Wxid, v.Name, v.Md5, v.AesKey, v.Size, v.Duration, v.CreatedAt)
	_, _ = s.db.Exec("DELETE FROM video_ring WHERE chat=? AND created_at<?", v.Chat, time.Now().Add(-woRingTTL).Unix())
	_, _ = s.db.Exec(`DELETE FROM video_ring WHERE chat=? AND msg_id NOT IN
		(SELECT msg_id FROM video_ring WHERE chat=? ORDER BY created_at DESC, msg_id DESC LIMIT ?)`,
		v.Chat, v.Chat, woRingKeep)
}

func (s *store) vringLatest(chat string) (ringVideo, bool) {
	return s.vringQuery("SELECT msg_id,chat,wxid,name,md5,aeskey,size,duration,created_at FROM video_ring WHERE chat=? ORDER BY created_at DESC, msg_id DESC LIMIT 1", chat)
}

func (s *store) vringBySender(chat, nameOrWxid string) (ringVideo, bool) {
	q := nameOrWxid
	return s.vringQuery(`SELECT msg_id,chat,wxid,name,md5,aeskey,size,duration,created_at FROM video_ring
		WHERE chat=? AND (wxid=? OR name=? OR name LIKE ?)
		ORDER BY created_at DESC, msg_id DESC LIMIT 1`, chat, q, q, q+"%")
}

func (s *store) vringByMsgID(chat string, msgID int64) (ringVideo, bool) {
	return s.vringQuery("SELECT msg_id,chat,wxid,name,md5,aeskey,size,duration,created_at FROM video_ring WHERE chat=? AND msg_id=?", chat, msgID)
}

func (s *store) vringQuery(q string, args ...any) (ringVideo, bool) {
	v := ringVideo{}
	if s == nil || s.db == nil {
		return v, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var size, dur int64
	err := s.db.QueryRow(q, args...).Scan(&v.MsgID, &v.Chat, &v.Wxid, &v.Name, &v.Md5, &v.AesKey, &size, &dur, &v.CreatedAt)
	if err != nil {
		return v, false
	}
	v.Size, v.Duration = uint32(size), uint32(dur)
	return v, true
}
