package student

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"placementhub/internal/audit"
	"placementhub/internal/auth"
	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/eligibility"
	"placementhub/internal/httpx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---- listing --------------------------------------------------------------

type ListItem struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Roll    string    `json:"roll"`
	Email   string    `json:"email"`
	Branch  string    `json:"branch"`
	CGPA    float64   `json:"cgpa"`
	Active  bool      `json:"active"`
	Status  string    `json:"status"`
	Company *string   `json:"company,omitempty"`
	CTC     *float64  `json:"ctc,omitempty"`
}

type ListFilter struct {
	Query  string
	Branch string
	Status string
	Limit  int
	Offset int
}

// escapeLike escapes LIKE wildcards so a search for "50%" matches literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (s *Service) List(ctx context.Context, f ListFilter) ([]ListItem, int, error) {
	pat := ""
	if q := strings.TrimSpace(f.Query); q != "" {
		pat = "%" + escapeLike(q) + "%"
	}
	base := fmt.Sprintf(`
FROM (SELECT s.id, u.name, s.roll, u.email, s.branch, s.cgpa, u.active, %s AS status
        FROM students s JOIN users u ON u.id = s.user_id) x
WHERE ($1 = '' OR x.name ILIKE $1 OR x.roll ILIKE $1 OR x.email ILIKE $1)
  AND ($2 = '' OR x.branch = $2)
  AND ($3 = '' OR x.status = $3)`, eligibility.StatusExpr("s"))

	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) "+base, pat, f.Branch, f.Status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT x.id, x.name, x.roll, x.email, x.branch, x.cgpa, x.active, x.status, po.company, po.ctc
  FROM (SELECT s.id, u.name, s.roll, u.email, s.branch, s.cgpa, u.active, `+eligibility.StatusExpr("s")+` AS status
          FROM students s JOIN users u ON u.id = s.user_id) x
  LEFT JOIN LATERAL (SELECT c.name AS company, o.ctc FROM offers o JOIN companies c ON c.id = o.company_id
                      WHERE o.student_id = x.id AND o.status = 'Accepted') po ON true
 WHERE ($1 = '' OR x.name ILIKE $1 OR x.roll ILIKE $1 OR x.email ILIKE $1)
   AND ($2 = '' OR x.branch = $2)
   AND ($3 = '' OR x.status = $3)
 ORDER BY x.roll
 LIMIT $4 OFFSET $5`, pat, f.Branch, f.Status, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []ListItem{}
	for rows.Next() {
		var it ListItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Roll, &it.Email, &it.Branch, &it.CGPA, &it.Active, &it.Status, &it.Company, &it.CTC); err != nil {
			return nil, 0, err
		}
		items = append(items, it)
	}
	return items, total, rows.Err()
}

// ---- create / import ------------------------------------------------------

type NewStudent struct {
	Name     string  `json:"name"`
	Roll     string  `json:"roll"`
	Email    string  `json:"email"`
	Branch   string  `json:"branch"`
	Year     string  `json:"year"`
	CGPA     float64 `json:"cgpa"`
	Backlogs int     `json:"backlogs"`
	Phone    string  `json:"phone"`
}

type Created struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Roll         string    `json:"roll"`
	Email        string    `json:"email"`
	TempPassword string    `json:"tempPassword"`
}

func (n *NewStudent) normalise() {
	n.Name = strings.TrimSpace(n.Name)
	n.Roll = strings.ToUpper(strings.TrimSpace(n.Roll))
	n.Email = strings.ToLower(strings.TrimSpace(n.Email))
	n.Year = strings.TrimSpace(n.Year)
	if n.Year == "" {
		n.Year = "Final year"
	}
	n.Phone = strings.TrimSpace(n.Phone)
}

func (n *NewStudent) validate() error {
	var v httpx.V
	v.Text("name", n.Name, 2, 120)
	v.Text("roll", n.Roll, 3, 30)
	v.Email("email", n.Email)
	v.OneOf("branch", n.Branch, domain.Branches...)
	v.Text("year", n.Year, 1, 30)
	v.Range("cgpa", n.CGPA, 0, 10)
	v.Range("backlogs", float64(n.Backlogs), 0, 50)
	if !phoneRE.MatchString(n.Phone) {
		v.Add("phone", "may contain only digits, spaces and + ( ) -")
	}
	return v.Err()
}

func (s *Service) insertStudent(ctx context.Context, n NewStudent, hash string) (uuid.UUID, error) {
	var sid uuid.UUID
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var uid uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (email, password_hash, role, name, must_change_password)
			 VALUES ($1, $2, 'student', $3, true) RETURNING id`, n.Email, hash, n.Name).Scan(&uid); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO students (user_id, roll, branch, year, cgpa, backlogs, phone)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			uid, n.Roll, n.Branch, n.Year, n.CGPA, n.Backlogs, n.Phone).Scan(&sid)
	})
	return sid, err
}

func duplicateError(err error) error {
	if db.PgCode(err) != db.CodeUniqueViolation {
		return err
	}
	if strings.Contains(db.PgConstraint(err), "roll") {
		return httpx.Conflict("roll_taken", "a student with this roll number already exists")
	}
	return httpx.Conflict("email_taken", "an account with this email already exists")
}

// Create adds one student with a generated temporary password. The password is
// returned once and must be changed at first login.
func (s *Service) Create(ctx context.Context, in NewStudent, actor uuid.UUID, ip string) (*Created, error) {
	in.normalise()
	if err := in.validate(); err != nil {
		return nil, err
	}
	pw, err := auth.TempPassword()
	if err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return nil, err
	}
	sid, err := s.insertStudent(ctx, in, hash)
	if err != nil {
		return nil, duplicateError(err)
	}
	audit.Log(ctx, s.pool, &actor, "student.created", "student", sid.String(), ip, map[string]any{"roll": in.Roll})
	return &Created{ID: sid, Name: in.Name, Roll: in.Roll, Email: in.Email, TempPassword: pw}, nil
}

const maxImportRows = 500

type RowError struct {
	Row     int    `json:"row"` // 1-based line in the file, header = line 1
	Message string `json:"message"`
}

type ImportResult struct {
	Created []Created  `json:"created"`
	Errors  []RowError `json:"errors"`
}

// Import creates students from CSV with columns
// name, roll, email, branch, cgpa (required) and year, backlogs, phone (optional).
// Valid rows are created even if others fail; every failure is reported per row.
func (s *Service) Import(ctx context.Context, r io.Reader, actor uuid.UUID, ip string) (*ImportResult, error) {
	cr := csv.NewReader(io.LimitReader(r, 2<<20))
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, httpx.BadRequest("the CSV file is empty or unreadable")
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, utf8BOM)))] = i
	}
	for _, need := range []string{"name", "roll", "email", "branch", "cgpa"} {
		if _, ok := col[need]; !ok {
			return nil, httpx.Unprocessable("bad_csv", "missing required column: "+need)
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	type pending struct {
		line int
		n    NewStudent
	}
	res := &ImportResult{Created: []Created{}, Errors: []RowError{}}
	var todo []pending
	seenEmail, seenRoll := map[string]bool{}, map[string]bool{}
	line := 1
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			res.Errors = append(res.Errors, RowError{line, "unreadable row"})
			continue
		}
		if len(todo)+len(res.Errors) >= maxImportRows {
			return nil, httpx.Unprocessable("too_many_rows", fmt.Sprintf("import at most %d students per file", maxImportRows))
		}
		n := NewStudent{Name: get(rec, "name"), Roll: get(rec, "roll"), Email: get(rec, "email"),
			Branch: get(rec, "branch"), Year: get(rec, "year"), Phone: get(rec, "phone")}
		var perr error
		if n.CGPA, perr = strconv.ParseFloat(get(rec, "cgpa"), 64); perr != nil {
			res.Errors = append(res.Errors, RowError{line, "cgpa must be a number"})
			continue
		}
		if b := get(rec, "backlogs"); b != "" {
			if n.Backlogs, perr = strconv.Atoi(b); perr != nil {
				res.Errors = append(res.Errors, RowError{line, "backlogs must be a whole number"})
				continue
			}
		}
		n.normalise()
		if verr := n.validate(); verr != nil {
			var he *httpx.Error
			msg := "invalid row"
			if errors.As(verr, &he) {
				parts := make([]string, 0, len(he.Fields))
				for f, m := range he.Fields {
					parts = append(parts, f+" "+m)
				}
				msg = strings.Join(parts, "; ")
			}
			res.Errors = append(res.Errors, RowError{line, msg})
			continue
		}
		if seenEmail[n.Email] || seenRoll[n.Roll] {
			res.Errors = append(res.Errors, RowError{line, "duplicate email or roll within the file"})
			continue
		}
		seenEmail[n.Email], seenRoll[n.Roll] = true, true
		todo = append(todo, pending{line, n})
	}

	// bcrypt is deliberately slow, so hash the temporary passwords in parallel.
	pws := make([]string, len(todo))
	hashes := make([]string, len(todo))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	var hashErr error
	var mu sync.Mutex
	for i := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			pw, err := auth.TempPassword()
			if err == nil {
				pws[i] = pw
				hashes[i], err = auth.HashPassword(pw)
			}
			if err != nil {
				mu.Lock()
				hashErr = err
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if hashErr != nil {
		return nil, hashErr
	}

	for i, p := range todo {
		sid, err := s.insertStudent(ctx, p.n, hashes[i])
		if err != nil {
			var he *httpx.Error
			if e := duplicateError(err); errors.As(e, &he) {
				res.Errors = append(res.Errors, RowError{p.line, he.Message})
				continue
			}
			return nil, err
		}
		res.Created = append(res.Created, Created{ID: sid, Name: p.n.Name, Roll: p.n.Roll, Email: p.n.Email, TempPassword: pws[i]})
	}
	audit.Log(ctx, s.pool, &actor, "student.imported", "student", "", ip,
		map[string]any{"created": len(res.Created), "errors": len(res.Errors)})
	return res, nil
}

// ---- admin edits ----------------------------------------------------------

// AdminUpdate holds the academic fields only the placement cell may change.
// Changing CGPA, branch or backlogs changes which jobs the student is eligible for.
type AdminUpdate struct {
	Name     *string  `json:"name"`
	Branch   *string  `json:"branch"`
	Year     *string  `json:"year"`
	CGPA     *float64 `json:"cgpa"`
	Backlogs *int     `json:"backlogs"`
}

func (s *Service) AdminUpdate(ctx context.Context, id uuid.UUID, in AdminUpdate, actor uuid.UUID, ip string) (*Profile, error) {
	var v httpx.V
	if in.Name != nil {
		v.Text("name", *in.Name, 2, 120)
	}
	if in.Branch != nil {
		v.OneOf("branch", *in.Branch, domain.Branches...)
	}
	if in.Year != nil {
		v.Text("year", *in.Year, 1, 30)
	}
	if in.CGPA != nil {
		v.Range("cgpa", *in.CGPA, 0, 10)
	}
	if in.Backlogs != nil {
		v.Range("backlogs", float64(*in.Backlogs), 0, 50)
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var uid uuid.UUID
		err := tx.QueryRow(ctx,
			`UPDATE students SET
			    branch = COALESCE($2, branch), year = COALESCE($3, year),
			    cgpa = COALESCE($4, cgpa), backlogs = COALESCE($5, backlogs), updated_at = now()
			  WHERE id = $1 RETURNING user_id`, id, in.Branch, trimPtr(in.Year), in.CGPA, in.Backlogs).Scan(&uid)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("student")
			}
			return err
		}
		if in.Name != nil {
			_, err = tx.Exec(ctx, `UPDATE users SET name = $2, updated_at = now() WHERE id = $1`, uid, strings.TrimSpace(*in.Name))
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	audit.Log(ctx, s.pool, &actor, "student.updated", "student", id.String(), ip, nil)
	return s.ByID(ctx, id)
}

// SetActive enables or disables a student's login. Disabling signs them out everywhere.
func (s *Service) SetActive(ctx context.Context, id uuid.UUID, active bool, actor uuid.UUID, ip string) error {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var uid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT user_id FROM students WHERE id = $1`, id).Scan(&uid); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("student")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET active = $2, updated_at = now() WHERE id = $1`, uid, active); err != nil {
			return err
		}
		if !active {
			_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, uid)
			return err
		}
		return nil
	})
	if err == nil {
		audit.Log(ctx, s.pool, &actor, "student.active_changed", "student", id.String(), ip, map[string]any{"active": active})
	}
	return err
}

// ResetPassword issues a new temporary password (there is no email channel).
func (s *Service) ResetPassword(ctx context.Context, id uuid.UUID, actor uuid.UUID, ip string) (string, error) {
	pw, err := auth.TempPassword()
	if err != nil {
		return "", err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return "", err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var uid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT user_id FROM students WHERE id = $1`, id).Scan(&uid); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("student")
			}
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE users SET password_hash = $2, must_change_password = true, updated_at = now() WHERE id = $1`, uid, hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, uid)
		return err
	})
	if err != nil {
		return "", err
	}
	audit.Log(ctx, s.pool, &actor, "student.password_reset", "student", id.String(), ip, nil)
	return pw, nil
}

// utf8BOM is written as bytes so no literal BOM character ends up in the source.
const utf8BOM = "\xef\xbb\xbf"
