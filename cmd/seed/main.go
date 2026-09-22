// Command seed loads development data that mirrors the frontend's lib/data.ts,
// so the UI can be pointed at a realistic backend straight away.
//
//	go run ./cmd/seed            # refuses to run twice or in production
//
// Logins created (development only!):
//
//	admin@college.edu            Admin@12345      placement cell
//	careers@nimbuslabs.example   Company@12345    recruiter (Nimbus Labs), and one per company
//	ananya.sharma@college.edu    Student@12345    student, and one per student
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	_ "time/tzdata"

	"placementhub/internal/auth"
	"placementhub/internal/config"
	"placementhub/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	adminPassword   = "Admin@12345"
	companyPassword = "Company@12345"
	studentPassword = "Student@12345"
)

type company struct {
	name, color, industry, hr, email, status string
}

type student struct {
	name, roll, branch string
	cgpa               float64
	placedAt           string // company name
	ctc                float64
}

type job struct {
	company, role, typ, location string
	ctc, minCGPA                 float64
	branches, skills             []string
	deadlineIn, openings         int
	desc                         string
}

func main() {
	log.SetFlags(0)
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.IsProd() {
		log.Fatal("refusing to seed a production database")
	}
	ctx := context.Background()
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		log.Fatal(err)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	var users int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users)
	if users > 0 {
		log.Fatalf("database already has %d users; seed only runs on an empty database", users)
	}
	if err := seed(ctx, pool, cfg); err != nil {
		log.Fatal(err)
	}
	fmt.Println("Seeded. Logins:")
	fmt.Printf("  admin    admin@college.edu            %s\n", adminPassword)
	fmt.Printf("  company  careers@nimbuslabs.example   %s\n", companyPassword)
	fmt.Printf("  student  ananya.sharma@college.edu    %s\n", studentPassword)
	fmt.Println("(every company and student in the sample data uses the same password for its role)")
}

func seed(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) error {
	hash := func(pw string) string {
		h, err := auth.HashPassword(pw)
		if err != nil {
			log.Fatal(err)
		}
		return h
	}
	adminHash, companyHash, studentHash := hash(adminPassword), hash(companyPassword), hash(studentPassword)

	return db.InTx(ctx, pool, func(tx pgx.Tx) error {
		exec := func(sql string, args ...any) error { _, err := tx.Exec(ctx, sql, args...); return err }
		fail := func(what string, err error) error { return fmt.Errorf("seed %s: %w", what, err) }

		var adminID uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO users (email, password_hash, role, name) VALUES ('admin@college.edu', $1, 'admin', 'Dr. Kavita Rao') RETURNING id`, adminHash).Scan(&adminID); err != nil {
			return fail("admin", err)
		}

		// ---- companies ----------------------------------------------------
		companies := []company{
			{"Nimbus Labs", "#4f46e5", "Cloud & Infrastructure", "Rohan Mehta", "careers@nimbuslabs.example", "Approved"},
			{"Quantra Systems", "#0891b2", "Logistics Tech", "Priya Nambiar", "campus@quantra.example", "Approved"},
			{"Finlytics", "#059669", "FinTech", "Amit Deshpande", "talent@finlytics.example", "Approved"},
			{"Helix Health", "#e11d48", "HealthTech / AI", "Dr. Sunita Rao", "hiring@helix.example", "Approved"},
			{"Zenith Motors", "#d97706", "Automotive", "Manoj Kumar", "campus@zenith.example", "Approved"},
			{"Orbit Cloud", "#7c3aed", "Cloud Services", "Neha Bhatt", "univ@orbitcloud.example", "Pending"},
			{"PixelForge", "#db2777", "Gaming & Design", "Sameer Khan", "jobs@pixelforge.example", "Pending"},
			{"DataBridge", "#2563eb", "Data Infrastructure", "Lakshmi Rao", "campus@databridge.example", "Approved"},
			{"Skyline Realty", "#64748b", "Real Estate", "Vivek Anand", "hr@skyline.example", "Rejected"},
		}
		cid := map[string]uuid.UUID{}
		for _, c := range companies {
			var uid uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO users (email, password_hash, role, name) VALUES ($1, $2, 'company', $3) RETURNING id`,
				c.email, companyHash, c.hr).Scan(&uid); err != nil {
				return fail("company user "+c.name, err)
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO companies (user_id, name, industry, hr_name, email, color, status, decided_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $7 <> 'Pending' THEN now() END) RETURNING id`,
				uid, c.name, c.industry, c.hr, c.email, c.color, c.status).Scan(&id); err != nil {
				return fail("company "+c.name, err)
			}
			cid[c.name] = id
		}

		// ---- students -----------------------------------------------------
		students := []student{
			{"Ananya Sharma", "21CS047", "CSE", 8.6, "", 0},
			{"Sneha Iyer", "21CS112", "CSE", 9.1, "Nimbus Labs", 18},
			{"Divya Menon", "21CS034", "CSE", 9.4, "Nimbus Labs", 20},
			{"Rahul Verma", "21IT018", "IT", 8.1, "", 0},
			{"Arjun Nair", "21EC061", "ECE", 7.6, "", 0},
			{"Meera Kapoor", "21CS089", "CSE", 8.8, "Helix Health", 22},
			{"Vikram Singh", "21IT072", "IT", 7.2, "", 0},
			{"Pooja Reddy", "21CS101", "CSE", 8.3, "", 0},
			{"Karan Malhotra", "21EC030", "ECE", 7.1, "Zenith Motors", 10},
			{"Ishaan Gupta", "21IT044", "IT", 8.0, "", 0},
			{"Tanvi Joshi", "21CS120", "CSE", 7.9, "Finlytics", 14},
			{"Aditya Rao", "21CS005", "CSE", 8.4, "", 0},
			{"Nikhil Bansal", "21EE027", "EEE", 7.4, "Zenith Motors", 9.5},
			{"Riya Sethi", "21ME052", "Mechanical", 7.8, "", 0},
			{"Harsh Patel", "21CE015", "Civil", 7.0, "", 0},
			{"Simran Kaur", "21EC090", "ECE", 8.5, "Quantra Systems", 12},
		}
		sid := map[string]uuid.UUID{}
		for _, s := range students {
			email := strings.ToLower(strings.ReplaceAll(s.name, " ", ".")) + "@college.edu"
			var uid uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO users (email, password_hash, role, name) VALUES ($1, $2, 'student', $3) RETURNING id`,
				email, studentHash, s.name).Scan(&uid); err != nil {
				return fail("student user "+s.name, err)
			}
			var id uuid.UUID
			skills := []string{"React", "TypeScript", "Node.js", "Python", "SQL"}
			if err := tx.QueryRow(ctx,
				`INSERT INTO students (user_id, roll, branch, cgpa, tenth, twelfth, phone, github, linkedin, about, skills)
				 VALUES ($1, $2, $3, $4, 92.5, 90.1, '+91 98765 43210', $5, $6, $7, $8) RETURNING id`,
				uid, s.roll, s.branch, s.cgpa, "github.com/"+strings.Split(email, "@")[0], "linkedin.com/in/"+strings.Split(email, "@")[0],
				"Final-year "+s.branch+" student who enjoys building full-stack products.", skills).Scan(&id); err != nil {
				return fail("student "+s.name, err)
			}
			sid[s.name] = id
			// A resume record so applying works; the file itself is not seeded.
			if err := exec(`INSERT INTO resumes (student_id, storage_key, filename, content_type, size_bytes)
			                VALUES ($1, $2, $3, 'application/pdf', 214000)`,
				id, uuid.NewString()+".pdf", strings.ReplaceAll(s.name, " ", "_")+"_Resume.pdf"); err != nil {
				return fail("resume", err)
			}
		}

		// ---- jobs ---------------------------------------------------------
		jobs := []job{
			{"Nimbus Labs", "Software Engineer", "Full-time", "Bengaluru", 18, 7.5, []string{"CSE", "IT", "ECE"}, []string{"Go", "Kubernetes", "Distributed Systems"}, 14, 12, "Build and scale the control plane that powers thousands of production clusters."},
			{"Quantra Systems", "Data Analyst", "Full-time", "Hyderabad", 12, 7.0, []string{"CSE", "IT", "ECE", "EEE"}, []string{"SQL", "Python", "Tableau"}, 17, 8, "Turn messy operational data into decisions for logistics customers across India."},
			{"Finlytics", "Frontend Developer", "Full-time", "Pune", 14, 7.5, []string{"CSE", "IT"}, []string{"React", "TypeScript", "Design Systems"}, 21, 6, "Own the customer-facing dashboards used by 200+ banks and NBFCs."},
			{"DataBridge", "Backend Engineer", "Full-time", "Gurugram", 16, 7.5, []string{"CSE", "IT"}, []string{"Node.js", "PostgreSQL", "Kafka"}, 40, 7, "Design high-throughput ingestion pipelines for enterprise integrations."},
			{"Helix Health", "ML Engineer", "Full-time", "Bengaluru", 22, 8.0, []string{"CSE", "IT", "ECE"}, []string{"PyTorch", "MLOps", "Computer Vision"}, 27, 4, "Ship diagnostic imaging models from research notebook to hospital deployment."},
			{"Zenith Motors", "Embedded Systems Engineer", "Full-time", "Chennai", 10, 7.0, []string{"ECE", "EEE", "Mechanical"}, []string{"C/C++", "RTOS", "CAN bus"}, 31, 15, "Work on firmware for the next generation of electric two-wheelers."},
			{"Nimbus Labs", "SRE Intern", "Internship", "Remote", 6, 7.0, []string{"CSE", "IT", "ECE"}, []string{"Linux", "Bash", "Monitoring"}, 29, 10, "Learn on-call, observability and incident response alongside senior SREs."},
		}
		jobID := map[string]uuid.UUID{}
		for _, j := range jobs {
			if companies[indexOf(companies, j.company)].status != "Approved" {
				continue
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO jobs (company_id, role, type, location, ctc, min_cgpa, branches, skills, deadline, openings, description, status, approved_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, ($9::timestamptz AT TIME ZONE $10)::date + $11::int, $12, $13, 'Open', now() - interval '3 days') RETURNING id`,
				cid[j.company], j.role, j.typ, j.location, j.ctc, j.minCGPA, j.branches, j.skills, time.Now(), cfg.Location.String(),
				j.deadlineIn, j.openings, j.desc).Scan(&id); err != nil {
				return fail("job "+j.role, err)
			}
			jobID[j.company+"/"+j.role] = id
		}
		// A draft, a pending job and a closed one for the recruiter/admin screens.
		if err := exec(`INSERT INTO jobs (company_id, role, type, location, ctc, min_cgpa, branches, skills, deadline, openings, description, status, submitted_at)
		                VALUES ($1, 'Technical Writer', 'Full-time', 'Remote', 9, 6.5, '{CSE,IT,ECE}', '{Writing,APIs}', current_date + 55, 2, 'Own developer documentation and API references.', 'Draft', NULL),
		                       ($1, 'Security Engineer', 'Full-time', 'Bengaluru', 20, 8.0, '{CSE,IT}', '{AppSec,Cryptography}', current_date - 11, 3, 'Secure the platform and partner with teams on threat modeling.', 'Closed', NULL)`,
			cid["Nimbus Labs"]); err != nil {
			return fail("extra jobs", err)
		}
		if err := exec(`INSERT INTO jobs (company_id, role, type, location, ctc, min_cgpa, branches, skills, deadline, openings, description, status, submitted_at)
		                VALUES ($1, 'Product Design Intern', 'Internship', 'Mumbai', 4.8, 6.5, '{CSE,IT}', '{Figma,Prototyping}', current_date + 30, 5, 'Design interfaces for games and creator tools.', 'Draft', NULL)`,
			cid["PixelForge"]); err != nil {
			return fail("pixelforge job", err)
		}

		// ---- applications for the demo student ---------------------------
		type app struct {
			job, stage, note string
		}
		ananya := sid["Ananya Sharma"]
		for _, a := range []app{
			{"Nimbus Labs/Software Engineer", "Interview", "Technical round · check your email for the slot"},
			{"Finlytics/Frontend Developer", "Shortlisted", "Awaiting interview slot"},
			{"Helix Health/ML Engineer", "Applied", ""},
			{"Quantra Systems/Data Analyst", "Rejected", "Not shortlisted after aptitude test"},
			{"DataBridge/Backend Engineer", "Applied", ""},
		} {
			if err := addApplication(ctx, tx, jobID[a.job], ananya, a.stage, a.note); err != nil {
				return fail("application "+a.job, err)
			}
		}
		// A live offer she can accept or decline.
		offerApp, err := addApplicationID(ctx, tx, jobID["Nimbus Labs/SRE Intern"], ananya, "Offered", "Offer valid for 7 days")
		if err != nil {
			return fail("offer application", err)
		}
		if err := exec(`INSERT INTO offers (application_id, student_id, job_id, company_id, ctc, valid_until)
		                VALUES ($1, $2, $3, $4, 6, now() + interval '6 days')`, offerApp, ananya, jobID["Nimbus Labs/SRE Intern"], cid["Nimbus Labs"]); err != nil {
			return fail("offer", err)
		}

		// Other applicants to Nimbus Labs, for the recruiter's board.
		for _, a := range []struct{ who, stage string }{
			{"Rahul Verma", "Shortlisted"}, {"Arjun Nair", "Applied"}, {"Aditya Rao", "Applied"}, {"Pooja Reddy", "Interview"},
		} {
			if err := addApplication(ctx, tx, jobID["Nimbus Labs/Software Engineer"], sid[a.who], a.stage, ""); err != nil {
				return fail("nimbus applicant "+a.who, err)
			}
		}

		// ---- placed students: accepted offers ----------------------------
		roleFor := map[string]string{"Nimbus Labs": "Nimbus Labs/Software Engineer", "Helix Health": "Helix Health/ML Engineer",
			"Zenith Motors": "Zenith Motors/Embedded Systems Engineer", "Finlytics": "Finlytics/Frontend Developer",
			"Quantra Systems": "Quantra Systems/Data Analyst"}
		for _, s := range students {
			if s.placedAt == "" {
				continue
			}
			jid := jobID[roleFor[s.placedAt]]
			appID, err := addApplicationID(ctx, tx, jid, sid[s.name], "Offered", "")
			if err != nil {
				return fail("placed application "+s.name, err)
			}
			// Insert as Pending first, then accept: the guard trigger only forbids new applications.
			var offerID uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO offers (application_id, student_id, job_id, company_id, ctc, valid_until, created_at)
				 VALUES ($1, $2, $3, $4, $5, now() + interval '30 days', now() - (random() * interval '20 days')) RETURNING id`,
				appID, sid[s.name], jid, cid[s.placedAt], s.ctc).Scan(&offerID); err != nil {
				return fail("placed offer "+s.name, err)
			}
			if err := exec(`UPDATE offers SET status = 'Accepted', responded_at = created_at + interval '1 day' WHERE id = $1`, offerID); err != nil {
				return fail("accept "+s.name, err)
			}
		}

		// ---- drives ------------------------------------------------------
		for _, d := range []struct {
			company, title string
			startsIn       time.Duration
			mode, venue    string
		}{
			{"Nimbus Labs", "SWE Technical Interviews", 3*24*time.Hour + 2*time.Hour, "On-campus", "Seminar Hall B"},
			{"Finlytics", "Frontend Coding Round", 6*24*time.Hour + 3*time.Hour, "On-campus", "Computer Lab 3"},
			{"Helix Health", "ML Case Study & Interviews", 10*24*time.Hour + time.Hour, "Virtual", "Zoom"},
			{"Zenith Motors", "Pool Campus Drive", 15*24*time.Hour + 4*time.Hour, "On-campus", "Main Auditorium"},
			{"Quantra Systems", "Analyst Aptitude Test", -3 * 24 * time.Hour, "On-campus", "Exam Block A"},
		} {
			if err := exec(`INSERT INTO drives (company_id, title, starts_at, mode, venue, min_cgpa, branches, created_by)
			                VALUES ($1, $2, $3, $4, $5, 7.0, '{CSE,IT,ECE}', $6)`,
				cid[d.company], d.title, time.Now().Add(d.startsIn), d.mode, d.venue, adminID); err != nil {
				return fail("drive "+d.title, err)
			}
		}

		// ---- notices and site content ------------------------------------
		for _, n := range []struct {
			ago        int
			tag, title string
		}{
			{2, "Result", "Placement Cell member selection results announced"},
			{3, "Drive", "Finlytics pool campus drive — registration closes soon"},
			{5, "Alert", "Beware of fake recruitment emails asking for fees or documents"},
			{9, "Drive", "Helix Health ML Engineer drive — eligibility and syllabus"},
			{13, "Event", "Orientation for final-year students: resume & aptitude workshop"},
			{19, "Drive", "Nimbus Labs SWE hiring — shortlist for technical round out"},
		} {
			if err := exec(`INSERT INTO notices (title, tag, published_at, created_by) VALUES ($1, $2, now() - make_interval(days => $3), $4)`,
				n.title, n.tag, n.ago, adminID); err != nil {
				return fail("notice", err)
			}
		}
		siteConfig, _ := json.Marshal(map[string]any{
			"name": "Placement Hub", "college": "Your College Name", "address": "College Road, Your Area, City, State - 000000",
			"email": "placements@yourcollege.edu", "phones": []string{"+91 00000 00000", "+91 00000 00001"},
			"hours": "Mon – Fri, 10:00 AM – 5:00 PM", "brochure": "#",
			"socials": []map[string]string{{"label": "LinkedIn", "href": "#"}, {"label": "Instagram", "href": "#"}},
			"deskMessages": []map[string]string{
				{"role": "Principal's desk", "name": "Prof. Firstname Lastname", "title": "Principal",
					"text": "Since its founding, our institution has stood for sincerity, integrity and excellence."},
				{"role": "Convenor's desk", "name": "Ms. Firstname Lastname", "title": "Convenor, Placement Cell",
					"text": "Education finds its true meaning when knowledge is translated into skills, confidence and meaningful career opportunities."},
			},
			"testimonials": []map[string]string{
				{"name": "Sneha Iyer", "batch": "2026 · CSE", "company": "Nimbus Labs", "quote": "The cell's mock interviews and resume reviews made the real thing feel familiar."},
			},
		})
		return exec(`INSERT INTO site_settings (key, value) VALUES ('config', $1)`, siteConfig)
	})
}

func addApplication(ctx context.Context, tx pgx.Tx, jobID, studentID uuid.UUID, stage, note string) error {
	_, err := addApplicationID(ctx, tx, jobID, studentID, stage, note)
	return err
}

func addApplicationID(ctx context.Context, tx pgx.Tx, jobID, studentID uuid.UUID, stage, note string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`INSERT INTO applications (job_id, student_id, stage, note, applied_at) VALUES ($1, $2, $3, $4, now() - (random() * interval '12 days'))
		 RETURNING id`, jobID, studentID, stage, note).Scan(&id)
	if err != nil {
		return id, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO application_events (application_id, from_stage, to_stage, note) VALUES ($1, NULL, $2, 'Seeded')`, id, stage)
	return id, err
}

func indexOf(cs []company, name string) int {
	for i, c := range cs {
		if c.name == name {
			return i
		}
	}
	log.Fatalf("unknown company %q", name)
	return -1
}
