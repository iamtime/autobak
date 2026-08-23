package repo

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Модель ключей.
//
//	master key (32 байта, случайный, свой на каждый сервер)
//	 ├─ chunk-id  : HMAC-SHA256 → идентификаторы чанков (дедупликация)
//	 ├─ chunk-data: XChaCha20-Poly1305 → содержимое чанков
//	 └─ meta      : XChaCha20-Poly1305 → снимки и индексы
//
// Master key никогда не лежит в репозитории открытым: он обёрнут паролем
// через Argon2id (файл keys/<id>) и продублирован recovery-кодом на бумаге.
// Отдельный ключ на сервер означает, что компрометация одного сервера не
// открывает бэкапы остальных.

const (
	keyLen        = 32
	infoChunkID   = "autobak/v1 chunk-id"
	infoChunkData = "autobak/v1 chunk-data"
	infoMeta      = "autobak/v1 meta"
	infoKeyID     = "autobak/v1 key-id"
)

var ErrWrongPassword = errors.New("autobak: неверный пароль или повреждённый файл ключа")

type ChunkID [32]byte

func (c ChunkID) String() string { return hex.EncodeToString(c[:]) }

// Prefix - первые два hex-символа, каталог для раскладки чанков по ФС.
func (c ChunkID) Prefix() string { return hex.EncodeToString(c[:1]) }

func ParseChunkID(s string) (ChunkID, error) {
	var id ChunkID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, fmt.Errorf("autobak: некорректный chunk id %q", s)
	}
	copy(id[:], b)
	return id, nil
}

type MasterKey struct {
	raw     []byte
	chunkID []byte
	data    *aead
	meta    *aead
}

func derive(master []byte, info string) []byte {
	k, err := hkdf.Key(sha256.New, master, nil, info, keyLen)
	if err != nil {
		// Возможно только при неверных константах длины - программная ошибка.
		panic("autobak: hkdf: " + err.Error())
	}
	return k
}

func NewMasterKey() (*MasterKey, error) {
	raw := make([]byte, keyLen)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("autobak: нет источника случайности: %w", err)
	}
	return LoadMasterKey(raw)
}

func LoadMasterKey(raw []byte) (*MasterKey, error) {
	if len(raw) != keyLen {
		return nil, fmt.Errorf("autobak: master key должен быть %d байт, получено %d", keyLen, len(raw))
	}
	k := &MasterKey{raw: append([]byte(nil), raw...)}
	k.chunkID = derive(k.raw, infoChunkID)
	var err error
	if k.data, err = newAEAD(derive(k.raw, infoChunkData)); err != nil {
		return nil, err
	}
	if k.meta, err = newAEAD(derive(k.raw, infoMeta)); err != nil {
		return nil, err
	}
	return k, nil
}

// ID - публичный отпечаток ключа. Позволяет проверить, что ключ подходит к
// репозиторию, не раскрывая сам ключ.
func (k *MasterKey) ID() string {
	return hex.EncodeToString(derive(k.raw, infoKeyID)[:8])
}

// ChunkID считается по открытому тексту, поэтому одинаковые данные дают
// одинаковый id и дедуплицируются. Ключ в HMAC не даёт тому, кто видит
// только репозиторий, проверить догадку «а лежит ли здесь вот этот файл».
func (k *MasterKey) ChunkID(plain []byte) ChunkID {
	return ChunkHasher{key: k.chunkID}.ID(plain)
}

// ChunkHasher вычисляет идентификаторы чанков, и только их.
//
// Существует ради агента: чтобы не гонять по сети то, что уже лежит в
// хранилище, агент должен уметь называть чанки теми же именами, что и
// десктоп. Расшифровать он при этом не может ничего - из четырёх ключей
// ему достаётся ровно один, и не тот, которым закрыты данные.
type ChunkHasher struct{ key []byte }

func NewChunkHasher(key []byte) (ChunkHasher, error) {
	if len(key) != keyLen {
		return ChunkHasher{}, fmt.Errorf("autobak: ключ идентификаторов должен быть %d байт", keyLen)
	}
	return ChunkHasher{key: append([]byte(nil), key...)}, nil
}

func (h ChunkHasher) ID(plain []byte) ChunkID {
	m := hmac.New(sha256.New, h.key)
	m.Write(plain)
	var id ChunkID
	copy(id[:], m.Sum(nil))
	return id
}

// ChunkIDKey отдаёт ключ идентификаторов для передачи агенту.
//
// Отдельный метод, а не публичное поле: это единственный ключ, который
// вообще покидает десктоп, и место его выдачи должно быть одно и легко
// находимое поиском.
func (k *MasterKey) ChunkIDKey() []byte {
	return append([]byte(nil), k.chunkID...)
}

// SealChunk привязывает шифротекст к id чанка: подменить один чанк другим,
// сохранив валидную подпись, нельзя.
func (k *MasterKey) SealChunk(id ChunkID, plain []byte) ([]byte, error) {
	return k.data.seal(id[:], plain)
}

func (k *MasterKey) OpenChunk(id ChunkID, ct []byte) ([]byte, error) {
	return k.data.open(id[:], ct)
}

// SealMeta шифрует снимки и индексы. context - имя объекта в репозитории,
// оно же дополнительные данные: файл, переставленный под другим именем,
// не расшифруется.
func (k *MasterKey) SealMeta(context string, plain []byte) ([]byte, error) {
	return k.meta.seal([]byte(context), plain)
}

func (k *MasterKey) OpenMeta(context string, ct []byte) ([]byte, error) {
	return k.meta.open([]byte(context), ct)
}

type aead struct {
	c interface {
		Seal(dst, nonce, plaintext, ad []byte) []byte
		Open(dst, nonce, ciphertext, ad []byte) ([]byte, error)
		NonceSize() int
		Overhead() int
	}
}

func newAEAD(key []byte) (*aead, error) {
	// XChaCha20 берётся ради 192-битного nonce: его можно брать случайным
	// без счётчика и не бояться повтора, что критично, когда чанки пишут
	// параллельно несколько горутин и несколько запусков агента.
	c, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("autobak: инициализация шифра: %w", err)
	}
	return &aead{c: c}, nil
}

func (a *aead) seal(ad, plain []byte) ([]byte, error) {
	ns := a.c.NonceSize()
	out := make([]byte, ns, ns+len(plain)+a.c.Overhead())
	if _, err := rand.Read(out[:ns]); err != nil {
		return nil, fmt.Errorf("autobak: нет источника случайности: %w", err)
	}
	return a.c.Seal(out, out[:ns], plain, ad), nil
}

func (a *aead) open(ad, ct []byte) ([]byte, error) {
	ns := a.c.NonceSize()
	if len(ct) < ns+a.c.Overhead() {
		return nil, errors.New("autobak: шифротекст обрезан")
	}
	out, err := a.c.Open(nil, ct[:ns], ct[ns:], ad)
	if err != nil {
		return nil, errors.New("autobak: не удалось расшифровать: неверный ключ или данные повреждены")
	}
	return out, nil
}

// --- Обёртка ключа паролем ------------------------------------------------

type KDFParams struct {
	Time    uint32 `json:"time"`
	MemoryK uint32 `json:"memory_kib"`
	Threads uint8  `json:"threads"`
}

// Ориентир: ~0.3 с и 64 МиБ на обычном ноутбуке. Дороже делать смысла мало -
// подбирать всё равно будут не пароль, а сам 256-битный ключ.
func DefaultKDFParams() KDFParams {
	return KDFParams{Time: 3, MemoryK: 64 * 1024, Threads: 4}
}

// validate проверяет параметры Argon2 до передачи в argon2.IDKey: нулевые
// Time/Threads вызывают там панику, чрезмерный MemoryK - исчерпание памяти.
func (p KDFParams) validate() error {
	if p.Time < 1 {
		return fmt.Errorf("autobak: KDF Time=%d недопустимо", p.Time)
	}
	if p.Threads < 1 {
		return fmt.Errorf("autobak: KDF Threads=%d недопустимо", p.Threads)
	}
	if p.MemoryK < 8 {
		return fmt.Errorf("autobak: KDF MemoryK=%d слишком мало", p.MemoryK)
	}
	// 4 ГиБ - потолок: законные параметры сильно ниже, а больше означает
	// либо ошибку, либо попытку вызвать нехватку памяти.
	if p.MemoryK > 4*1024*1024 {
		return fmt.Errorf("autobak: KDF MemoryK=%d слишком велико", p.MemoryK)
	}
	return nil
}

type KeyFile struct {
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Hint    string    `json:"hint,omitempty"`
	Created time.Time `json:"created"`
	KDF     string    `json:"kdf"`
	Params  KDFParams `json:"params"`
	Salt    []byte    `json:"salt"`
	Sealed  []byte    `json:"sealed"`
}

func WrapMasterKey(k *MasterKey, password, hint string, p KDFParams, now time.Time) (*KeyFile, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("autobak: нет источника случайности: %w", err)
	}
	kek, err := newAEAD(argon2.IDKey([]byte(password), salt, p.Time, p.MemoryK, p.Threads, keyLen))
	if err != nil {
		return nil, err
	}
	sealed, err := kek.seal([]byte(k.ID()), k.raw)
	if err != nil {
		return nil, err
	}
	return &KeyFile{
		Version: 1, ID: k.ID(), Hint: hint, Created: now.UTC(),
		KDF: "argon2id", Params: p, Salt: salt, Sealed: sealed,
	}, nil
}

func UnwrapMasterKey(kf *KeyFile, password string) (*MasterKey, error) {
	if kf.KDF != "argon2id" {
		return nil, fmt.Errorf("autobak: неизвестный KDF %q", kf.KDF)
	}
	// Параметры приходят из файла ключа, а его при желании может подложить
	// сервер, имеющий право записи. Без проверки Time=0 или Threads=0
	// уронили бы argon2.IDKey паникой, а огромный MemoryK - выел бы память.
	if err := kf.Params.validate(); err != nil {
		return nil, err
	}
	kek, err := newAEAD(argon2.IDKey([]byte(password), kf.Salt, kf.Params.Time, kf.Params.MemoryK, kf.Params.Threads, keyLen))
	if err != nil {
		return nil, err
	}
	raw, err := kek.open([]byte(kf.ID), kf.Sealed)
	if err != nil {
		return nil, ErrWrongPassword
	}
	k, err := LoadMasterKey(raw)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(k.ID()), []byte(kf.ID)) != 1 {
		return nil, ErrWrongPassword
	}
	return k, nil
}

// --- Recovery-код ---------------------------------------------------------

var recEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// RecoveryCode - тот же master key в виде, который можно распечатать и убрать
// в сейф. Это единственный путь к бэкапам, если утрачены и ПК, и пароль.
func (k *MasterKey) RecoveryCode() string {
	s := recEnc.EncodeToString(k.raw)
	var b strings.Builder
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+4, len(s))])
	}
	return b.String()
}

var recCleaner = strings.NewReplacer("-", "", " ", "", "\n", "", "\r", "", "\t", "")

func ParseRecoveryCode(code string) (*MasterKey, error) {
	raw, err := recEnc.DecodeString(strings.ToUpper(recCleaner.Replace(code)))
	if err != nil {
		return nil, fmt.Errorf("autobak: recovery-код испорчен: %w", err)
	}
	return LoadMasterKey(raw)
}
