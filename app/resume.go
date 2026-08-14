package main

// The resume, as data. Both the rendered timeline and the "spec" YAML view
// (see yaml.go) are derived from this single source, so they can't drift
// apart the way a hand-duplicated version would.

// SubRole is one promotion within a longer, single-company stretch of a
// career (see Oracle Cerner below) - detailed enough for the expandable
// rendered view, deliberately excluded from the spec view where a single
// "promotions" count says the same thing more concisely.
type SubRole struct {
	Title string
	From  string
	To    string
	Blurb string
}

// Experience is one entry in the timeline. Current is used only for the
// rail's "current role" styling. SubRoles is nil for a standalone role;
// when set, Title/From/To describe the whole span and SubRoles breaks it
// into the individual promotions.
type Experience struct {
	Title      string
	Company    string
	From       string
	To         string // "present" for the current role
	Current    bool
	Blurb      string // one line, used in the homepage's condensed preview
	Bullets    []string
	Skills     []string // displayed as badges on the resume page
	FilterTags []string // the (possibly broader) set the skill filter matches against
	Promotions int      // >0 only for a grouped, multi-promotion entry
	SubRoles   []SubRole
}

// Education is one degree.
type Education struct {
	Degree string
	School string
	Year   string
	Detail string // shown only in the rendered view
}

// Resume is the whole document.
type Resume struct {
	Name       string
	Title      string
	Bio        string
	Stack      []string // homepage "stack" tags
	Experience []Experience
	Education  []Education
	HomeLab    string
}

var resume = Resume{
	Name:  "Rob Cameron",
	Title: "Senior Site Reliability Engineer",
	Bio:   "Twenty years in IT, the last several spent moving from keeping systems running to designing the platforms that keep themselves running. This site is both the resume and a live instance of that work - the stats below are read from the cluster it's deployed on.",
	Stack: []string{
		"Kubernetes", "Terraform", "Crossplane", "Argo CD", "Go", "Azure",
		"Prometheus / Grafana", "GitHub Actions",
	},
	Experience: []Experience{
		{
			Title:   "Senior Site Reliability Engineer",
			Company: "iManage",
			From:    "2021-05",
			To:      "present",
			Current: true,
			Blurb:   "Azure landing zones, a Crossplane self-service platform, and the Go tooling that reconciles infrastructure state.",
			Bullets: []string{
				"Co-founded a self-organized team redesigning cloud architecture on Azure landing zone best practices - blue/green upgrades now take minutes instead of hours.",
				"Designed HA, regionally redundant Teleport via Terraform for zero-trust access to AKS, with Entra ID SSO.",
				"Built a Crossplane proof of concept and custom APIs for self-service, FIPS/FedRAMP-compliant Kubernetes clusters via pull request; now developing the Go functions and CI/CD for production.",
				"Writes Go automation, including a Kubernetes controller that reconciles enabled services with Azure Firewall DNAT rules and Traffic Manager IP groups.",
			},
			Skills:     []string{"Kubernetes", "Terraform", "Go", "Crossplane"},
			FilterTags: []string{"Kubernetes", "Terraform", "Go", "Automation"},
		},
		{
			Title:   "Senior Database Administrator",
			Company: "Oracle Cerner",
			From:    "2018-05",
			To:      "2021-05",
			Blurb:   "SQL tuning, automation, and alerting across Oracle Cerner's client-hosted databases.",
			Bullets: []string{
				"SQL tuning that cut thousands of 5+ second clinician interactions a month to under 0.1s, across every client site.",
				"Automated log parsing, data pump operations, and diagnostic collection; rebuilt alert logic to cut false page-outs for the whole DBA team.",
			},
			Skills:     []string{"Automation", "SQL tuning"},
			FilterTags: []string{"Automation"},
		},
		{
			Title:      "System Engineer .. Senior System Engineer",
			Company:    "Oracle Cerner",
			From:       "2005-01",
			To:         "2018-05",
			Promotions: 5,
			Skills:     []string{"Leadership", "Automation"},
			FilterTags: []string{"Leadership", "Automation"},
			SubRoles: []SubRole{
				{
					Title: "Senior System Engineer",
					From:  "2016-11",
					To:    "2018-05",
					Blurb: "Led System Engineer community meetings for 120+ engineers; became the Linux/shell scripting SME; automated printer config and batch user creation.",
				},
				{
					Title: "Technical Engagement Leader",
					From:  "2014-02",
					To:    "2016-11",
					Blurb: "Main client point of contact; led a team of 5-6 engineers through multi-month and year-long implementations.",
				},
				{
					Title: "Senior System Engineer",
					From:  "2012-05",
					To:    "2014-02",
					Blurb: "Managed RHEL systems and AIX/HP-UX to RHEL migrations for Cerner Millennium.",
				},
				{
					Title: "Team Lead",
					From:  "2009-04",
					To:    "2012-05",
					Blurb: "Managed 4-5 engineers building and supporting IQHealth, Cerner's patient portal.",
				},
				{
					Title: "System Engineer",
					From:  "2005-01",
					To:    "2009-04",
					Blurb: "Scripted the front and back-end build process for implementation projects, cutting effort from 200 hours to 40.",
				},
			},
		},
	},
	Education: []Education{
		{
			Degree: "BS Computer Information Technology",
			School: "Purdue University",
			Year:   "2004",
			Detail: "Minor in Organizational Leadership - 3.55 GPA",
		},
	},
	HomeLab: "A Salt-managed Linux cluster for learning in public: Stable Diffusion/Flux training, a self-hosted game server managed via GitHub issues, and a load-balanced Kubernetes deployment.",
}
