// Package student covers a student's own profile, skills and resume, plus the
// placement cell's management of student records.
package student

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"placementhub/internal/db"
	"placementhub/internal/eligibility"
	"placementhub/internal/httpx"
	"placementhub/internal/storage"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool         *pgxpool.Pool
	store        storage.Storage
	maxResumeLen int64
}

func NewService(pool *pgxpool.Pool, store storage.Storage, maxResumeBytes int64) *Service {
	return &Service{pool: pool, store: store, maxResumeLen: maxResumeBytes}
}

// ---- profile --------------------------------------------------------------

type ResumeMeta struct {
	Filename   string    `json:"filename"`
	SizeBytes  int64     `json:"sizeBytes"`
	UploadedAt time.Time `json:"uploadedAt"`
}

type Placement struct {
	Status  string   `json:"status"` // Placed | In process | Unplaced
	OfferID *string  `json:"offerId,omitempty"`
	Company *string  `json:"company,omitempty"`
	Role    *string  `json:"role,omitempty"`
	CTC     *float64 `json:"ctc,omitempty"`
}

type Links struct {
	GitHub   string `json:"github"`
	LinkedIn string `json:"linkedin"`
}

type Profile struct {
	ID        uuid.UUID   `json:"id"`
	Name      string      `json:"name"`
	Roll      string      `json:"roll"`
	Email     string      `json:"email"`
	Phone     string      `json:"phone"`
	Branch    string      `json:"branch"`
	Year      string      `json:"year"`
	CGPA      float64     `json:"cgpa"`
	Backlogs  int         `json:"backlogs"`
	Tenth     *float64    `json:"tenth"`
	Twelfth   *float64    `json:"twelfth"`
	Skills    []string    `json:"skills"`
	Links     Links       `json:"links"`
	About     string      `json:"about"`
	Resume    *ResumeMeta `json:"resume"`
	Placement Placement   `json:"placement"`
}

const profileSelect = `
SELECT s.id, u.name, s.roll, u.email, s.phone, s.branch, s.year, s.cgpa, s.backlogs, s.tenth, s.twelfth,
       s.skills, s.github, s.linkedin, s.about,
       r.filename, r.size_bytes, r.uploaded_at,
       %[1]s AS status,
       po.id::text, po.company, po.role, po.ctc
  FROM students s
  JOIN users u ON u.id = s.user_id
  LEFT JOIN resumes r ON r.student_id = s.id
  LEFT JOIN LATERAL (
        SELECT o.id, c.name AS company, j.role, o.ctc
          FROM offers o JOIN jobs j ON j.id = o.job_id JOIN companies c ON c.id = o.company_id
         WHERE o.student_id = s.id AND o.status = 'Accepted') po ON true
 WHERE %[2]s`

func scanProfile(row pgx.Row) (*Profile, error) {
	var (
		p                      Profile
		rf                     *string
		rs                     *int64
		ru                     *time.Time
		offerID, company, role *string
		ctc                    *float64
	)
	err := row.Scan(&p.ID, &p.Name, &p.Roll, &p.Email, &p.Phone, &p.Branch, &p.Year, &p.CGPA, &p.Backlogs,
		&p.Tenth, &p.Twelfth, &p.Skills, &p.Links.GitHub, &p.Links.LinkedIn, &p.About,
		&rf, &rs, &ru, &p.Placement.Status, &offerID, &company, &role, &ctc)
	if err != nil {
		return nil, err
	}
	if rf != nil {
		p.Resume = &ResumeMeta{Filename: *rf, SizeBytes: *rs, UploadedAt: *ru}
	}
	p.Placement.OfferID, p.Placement.Company, p.Placement.Role, p.Placement.CTC = offerID, company, role, ctc
	if p.Skills == nil {
		p.Skills = []string{}
	}
	return &p, nil
}

// ByUser loads the profile of the signed-in student.
func (s *Service) ByUser(ctx context.Context, userID uuid.UUID) (*Profile, error) {
	p, err := scanProfile(s.pool.QueryRow(ctx,
		fmt.Sprintf(profileSelect, eligibility.StatusExpr("s"), "s.user_id = $1"), userID))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("student profile")
		}
		return nil, err
	}
	return p, nil
}

// ByID loads any student (placement cell view).
func (s *Service) ByID(ctx context.Context, id uuid.UUID) (*Profile, error) {
	p, err := scanProfile(s.pool.QueryRow(ctx,
		fmt.Sprintf(profileSelect, eligibility.StatusExpr("s"), "s.id = $1"), id))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("student")
		}
		return nil, err
	}
	return p, nil
}

// StudentID resolves the student record behind a user.
func (s *Service) StudentID(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM students WHERE user_id = $1`, userID).Scan(&id)
	if db.IsNoRows(err) {
		return uuid.Nil, httpx.NotFound("student profile")
	}
	return id, err
}

// ProfileUpdate lists only what a student may edit. Name, roll, email, branch,
// CGPA and backlogs are set by the placement cell, so they are absent here and
// any such fields in the request body are ignored.
type ProfileUpdate struct {
	Phone    *string  `json:"phone"`
	About    *string  `json:"about"`
	Tenth    *float64 `json:"tenth"`
	Twelfth  *float64 `json:"twelfth"`
	Links    *Links   `json:"links"`
	GitHub   *string  `json:"github"`
	LinkedIn *string  `json:"linkedin"`
}

var phoneRE = regexp.MustCompile(`^[0-9+()\- ]{0,30}$`)

func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, in ProfileUpdate) (*Profile, error) {
	if in.Links != nil { // accept the nested shape the UI already uses
		if in.GitHub == nil {
			in.GitHub = &in.Links.GitHub
		}
		if in.LinkedIn == nil {
			in.LinkedIn = &in.Links.LinkedIn
		}
	}
	var v httpx.V
	if in.Phone != nil && !phoneRE.MatchString(*in.Phone) {
		v.Add("phone", "may contain only digits, spaces and + ( ) -")
	}
	if in.About != nil {
		v.Text("about", *in.About, 0, 2000)
	}
	if in.GitHub != nil {
		v.Text("github", *in.GitHub, 0, 200)
	}
	if in.LinkedIn != nil {
		v.Text("linkedin", *in.LinkedIn, 0, 200)
	}
	if in.Tenth != nil {
		v.Range("tenth", *in.Tenth, 0, 100)
	}
	if in.Twelfth != nil {
		v.Range("twelfth", *in.Twelfth, 0, 100)
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE students SET
		    phone    = COALESCE($2, phone),
		    about    = COALESCE($3, about),
		    tenth    = COALESCE($4, tenth),
		    twelfth  = COALESCE($5, twelfth),
		    github   = COALESCE($6, github),
		    linkedin = COALESCE($7, linkedin),
		    updated_at = now()
		  WHERE user_id = $1`,
		userID, trimPtr(in.Phone), trimPtr(in.About), in.Tenth, in.Twelfth, trimPtr(in.GitHub), trimPtr(in.LinkedIn))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("student profile")
	}
	return s.ByUser(ctx, userID)
}

func trimPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

// ---- skills ---------------------------------------------------------------

const maxSkills = 30

func normalizeSkill(raw string) (string, bool) {
	s := strings.Join(strings.Fields(raw), " ")
	if s == "" || len([]rune(s)) > 40 {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

func (s *Service) AddSkill(ctx context.Context, userID uuid.UUID, raw string) ([]string, error) {
	skill, ok := normalizeSkill(raw)
	if !ok {
		return nil, httpx.Unprocessable("invalid_skill", "a skill must be 1-40 characters")
	}
	var skills []string
	err := s.pool.QueryRow(ctx,
		`UPDATE students SET
		    skills = CASE
		      WHEN lower($2) = ANY (SELECT lower(x) FROM unnest(skills) x) THEN skills
		      ELSE array_append(skills, $2) END,
		    updated_at = now()
		  WHERE user_id = $1 AND (cardinality(skills) < $3 OR lower($2) = ANY (SELECT lower(x) FROM unnest(skills) x))
		  RETURNING skills`, userID, skill, maxSkills).Scan(&skills)
	if db.IsNoRows(err) {
		var exists bool
		if e := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM students WHERE user_id = $1)`, userID).Scan(&exists); e != nil {
			return nil, e
		}
		if !exists {
			return nil, httpx.NotFound("student profile")
		}
		return nil, httpx.Unprocessable("too_many_skills", fmt.Sprintf("you can list at most %d skills", maxSkills))
	}
	return skills, err
}

func (s *Service) RemoveSkill(ctx context.Context, userID uuid.UUID, raw string) ([]string, error) {
	var skills []string
	err := s.pool.QueryRow(ctx,
		`UPDATE students SET
		    skills = ARRAY(SELECT x FROM unnest(skills) WITH ORDINALITY AS t(x, n) WHERE lower(x) <> lower($2) ORDER BY n),
		    updated_at = now()
		  WHERE user_id = $1 RETURNING skills`, userID, strings.TrimSpace(raw)).Scan(&skills)
	if db.IsNoRows(err) {
		return nil, httpx.NotFound("student profile")
	}
	return skills, err
}

// ---- resume ---------------------------------------------------------------

var pdfMagic = []byte("%PDF-")

// SaveResume validates and stores a PDF, replacing any earlier one.
func (s *Service) SaveResume(ctx context.Context, userID uuid.UUID, filename string, r io.Reader) (*ResumeMeta, error) {
	sid, err := s.StudentID(ctx, userID)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(r, s.maxResumeLen+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxResumeLen {
		return nil, httpx.NewError(http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("resume must be at most %d MB", s.maxResumeLen>>20))
	}
	if len(data) < 100 || !bytes.HasPrefix(data, pdfMagic) || http.DetectContentType(data) != "application/pdf" {
		return nil, httpx.Unprocessable("invalid_file", "resume must be a PDF file")
	}

	key := uuid.NewString() + ".pdf"
	if _, err := s.store.Put(ctx, key, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	name := SafeFilename(filename)
	var oldKey *string
	var meta ResumeMeta
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, `SELECT storage_key FROM resumes WHERE student_id = $1 FOR UPDATE`, sid).Scan(&oldKey)
		return tx.QueryRow(ctx,
			`INSERT INTO resumes (student_id, storage_key, filename, content_type, size_bytes)
			 VALUES ($1, $2, $3, 'application/pdf', $4)
			 ON CONFLICT (student_id) DO UPDATE
			   SET storage_key = EXCLUDED.storage_key, filename = EXCLUDED.filename,
			       size_bytes = EXCLUDED.size_bytes, uploaded_at = now()
			 RETURNING filename, size_bytes, uploaded_at`, sid, key, name, len(data)).
			Scan(&meta.Filename, &meta.SizeBytes, &meta.UploadedAt)
	})
	if err != nil {
		_ = s.store.Delete(ctx, key)
		return nil, err
	}
	if oldKey != nil {
		_ = s.store.Delete(ctx, *oldKey)
	}
	return &meta, nil
}

// SafeFilename keeps a display name that is safe in headers and UIs.
func SafeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsControl(r), r == '"', r == '/', r == '\\', r == ';':
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimSuffix(out, filepath.Ext(out))
	if out == "" || out == "." {
		out = "resume"
	}
	if r := []rune(out); len(r) > 80 {
		out = string(r[:80])
	}
	return out + ".pdf"
}

// Resume is an open resume file.
type Resume struct {
	Filename string
	Size     int64
	Body     io.ReadCloser
}

// OpenResume streams a student's own resume.
func (s *Service) OpenResume(ctx context.Context, studentID uuid.UUID) (*Resume, error) {
	var key, filename string
	var size int64
	err := s.pool.QueryRow(ctx, `SELECT storage_key, filename, size_bytes FROM resumes WHERE student_id = $1`, studentID).
		Scan(&key, &filename, &size)
	if db.IsNoRows(err) {
		return nil, httpx.NotFound("resume")
	}
	if err != nil {
		return nil, err
	}
	body, err := s.store.Open(ctx, key)
	if err != nil {
		if err == storage.ErrNotFound {
			return nil, httpx.NotFound("resume")
		}
		return nil, err
	}
	return &Resume{Filename: filename, Size: size, Body: body}, nil
}
