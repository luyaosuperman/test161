package test161

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/satori/go.uuid"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

type SubmissionUserInfo struct {
	Email   string `yaml:"email"`
	Token   string `yaml:"token"`
	KeyHash string `yaml:"-"`
}

// SubmissionRequests are created by clients and used to generate Submissions.
// A SubmissionRequest represents the data required to run a test161 target
// for evaluation by the test161 server.
type SubmissionRequest struct {
	Target          string                // Name of the target
	Users           []*SubmissionUserInfo // Email addresses of users
	Repository      string                // Git repository to clone
	CommitID        string                // Git commit id to checkout after cloning
	CommitRef       string                // The ref they're submitting with, if one is set
	ClientVersion   ProgramVersion        // The version of test161 the client is running
	EstimatedScores map[string]uint       // The local score test161 computed
}

// UploadRequests are created by clients and provide the form fields for
// file uploads. Currently, we only support stats file uploads, but this
// could change.
type UploadRequest struct {
	UploadType int
	Users      []*SubmissionUserInfo
}

const (
	UPLOAD_TYPE_USAGE = iota
)

const (
	SUBMISSION_SUBMITTED = "submitted" // Submitted and queued
	SUBMISSION_BUILDING  = "building"  // Building the kernel
	SUBMISSION_RUNNING   = "running"   // The tests started running
	SUBMISSION_ABORTED   = "aborted"   // Aborted because one or more tests failed to error
	SUBMISSION_COMPLETED = "completed" // Completed
)

type Submission struct {

	// Configuration
	ID            string   `bson:"_id,omitempty"`
	Users         []string `bson:"users"`
	Repository    string   `bson:"repository"`
	CommitID      string   `bson:"commit_id"`
	CommitRef     string   `bson:"commit_ref"`     // Just informational
	ClientVersion string   `bson:"client_version"` // Just informational

	// From the environment
	OverlayCommitID string `bson:"overlay_commit_id"` // Just informational
	IsStaff         bool   `bson:"is_staff"`

	// Target details
	TargetID      string `bson:"target_id"`
	TargetName    string `bson:"target_name"`
	TargetVersion uint   `bson:"target_version"`
	IsMetaTarget  bool   `bson:"is_meta_target"`

	// Submitted target, which is different from target details if submitting
	// to a subtarget of a metatarget.
	SubmittedTargetID      string `bson:"submitted_target_id"`
	SubmittedTargetName    string `bson:"submitted_target_name"`
	SubmittedTargetVersion uint   `bson:"submitted_target_version"`

	PointsAvailable uint   `bson:"max_score"`
	TargetType      string `bson:"target_type"`

	// Results
	Status         string   `bson:"status"`
	Score          uint     `bson:"score"`
	Performance    float64  `bson:"performance"`
	TestIDs        []string `bson:"tests"`
	Errors         []string `bson:"errors"`
	EstimatedScore uint     `bson:"estimated_score"`

	SubmissionTime time.Time `bson:"submission_time"`
	CompletionTime time.Time `bson:"completion_time"`

	Env *TestEnvironment `bson:"-" json:"-"`

	BuildTest *BuildTest `bson:"-" json:"-"`
	Tests     *TestGroup `bson:"-" json:"-"`

	students []*Student

	// Split information for meta/sub-targets. We store IDs for
	// mongo/persistence, and keep references around in case we need them,
	// and for testing.
	OrigSubmissionID string `bson:"orig_submission_id"`
	origTarget       *Target
	SubSubmissionIDs []string `bson:"sub_submission_ids"`
	subSubmissions   map[string]*Submission

	// From the request, but we need it in case we split the submission.
	estimatedScores map[string]uint
}

type TargetStats struct {
	TargetName    string `bson:"target_name"`
	TargetVersion uint   `bson:"target_version"`
	TargetType    string `bson:"target_type"`
	MaxScore      uint   `bson:"max_score"`

	TotalSubmissions uint `bson:"total_submissions"`
	TotalComplete    uint `bson:"total_complete"`

	HighScore uint    `bson:"high_score"`
	LowScore  uint    `bson:"low_score"`
	AvgScore  float64 `bson:"avg_score"`

	BestPerf  float64 `bson:"best_perf"`
	WorstPerf float64 `bson:"worst_perf"`
	AvgPerf   float64 `bson:"avg_perf"`

	BestSubmission string `bson:"best_submission_id"`
}

type Student struct {
	ID        string `bson:"_id"`
	Email     string `bson:"email"`
	Token     string `bson:"token"`
	PublicKey string `bson:"key"`

	// Stats
	TotalSubmissions uint           `bson:"total_submissions"`
	Stats            []*TargetStats `bson:"target_stats"`

	// Computed, cached.
	// 0 == uncached, 1 == false, 2 == true
	isStaff int
}

// Target stats sorting
type StatsByName []*TargetStats

func (a StatsByName) Len() int           { return len(a) }
func (a StatsByName) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a StatsByName) Less(i, j int) bool { return a[i].TargetName < a[j].TargetName }

// Keep track of pending submissions.  Keep this out of the database in case there are
// communication issues so that we don't need to manually reset things in the DB.
var userLock = &sync.Mutex{}
var pendingSubmissions = make(map[string]bool)

// Check users against users database.  Don't lock them until we run though
func validateUserRecords(users []*SubmissionUserInfo, env *TestEnvironment) ([]*Student, error) {

	allStudents := make([]*Student, 0)

	for _, user := range users {

		if students, err := getStudents(user.Email, user.Token, env); err != nil {
			return nil, err
		} else {
			allStudents = append(allStudents, students[0])
		}
	}

	return allStudents, nil
}

// Get a particular student from the DB and validate
func getStudents(email, token string, env *TestEnvironment) ([]*Student, error) {

	request := map[string]interface{}{
		"email": email,
		"token": token,
	}
	students := []*Student{}
	if err := env.Persistence.Retrieve(PERSIST_TYPE_STUDENTS, request, nil, &students); err != nil {
		return nil, err
	}

	if len(students) != 1 || students[0].Email != email || students[0].Token != token {
		return nil, errors.New("Unable to authenticate student: " + email)

	}

	return students, nil
}

func validateUsers(users []*SubmissionUserInfo, env *TestEnvironment) ([]*Student, error) {
	students := []*Student{}
	var err error

	if len(users) == 0 {
		return students, errors.New("No usernames specified")
	}

	if env.Persistence != nil && env.Persistence.CanRetrieve() {
		if students, err = validateUserRecords(users, env); err != nil {
			return students, err
		}
	}

	// Staff flag needs to be all or nothing
	staffCnt := 0
	for _, student := range students {
		if staff, err := student.IsStaff(env); err != nil {
			return students, err
		} else if staff {
			staffCnt += 1
		}
	}
	if staffCnt > 0 && staffCnt < len(students) {
		return students, errors.New("Cannot have staff working with students!")
	}

	return students, nil

}

func (req *SubmissionRequest) Validate(env *TestEnvironment) ([]*Student, error) {

	students := []*Student{}
	var err error

	// Non-user checks

	if strings.HasPrefix(req.Repository, "http") {
		return students, errors.New("test161 must access your repository via SSH and does not accept submissions from http/https URLs")
	}

	if _, ok := env.Targets[req.Target]; !ok {
		return students, errors.New("Invalid target: " + req.Target)
	}

	if len(req.Repository) == 0 || len(req.CommitID) == 0 {
		return students, errors.New("Must specify a Git repository and commit id")
	}

	// User checks (shared)
	students, err = validateUsers(req.Users, env)

	return students, err
}

func (req *UploadRequest) Validate(env *TestEnvironment) ([]*Student, error) {
	return validateUsers(req.Users, env)
}

// Create a new Submission that can be evaluated by the test161 server or client.
//
// This submission has a copy of the test environment, so it's safe to pass the
// same enviromnent for multiple submissions. Local fields will be set accordingly.
func NewSubmission(request *SubmissionRequest, origenv *TestEnvironment) (*Submission, []error) {
	var students []*Student
	var err error

	env := origenv.CopyEnvironment()

	// Validate the request details and get the list of students for which
	// this submission applies. We'll use this list later when we
	// actually run the submission.
	if students, err = request.Validate(env); err != nil {
		return nil, []error{err}
	}

	// (The target was validated in the previous step)
	target := env.Targets[request.Target]

	// Create the build configuration.  This is a combination of
	// the environment, target, and request.
	conf := &BuildConf{}
	conf.Repo = request.Repository
	conf.CommitID = request.CommitID
	conf.CacheDir = env.CacheDir
	conf.KConfig = target.KConfig
	conf.RequiredCommit = target.RequiredCommit
	conf.RequiresUserland = target.RequiresUserland
	conf.Overlay = target.Name

	conf.Users = make([]string, 0, len(request.Users))
	for _, u := range request.Users {
		conf.Users = append(conf.Users, u.Email)
	}

	// Add first 'test' (build)
	buildTest, err := conf.ToBuildTest(env)
	if err != nil {
		return nil, []error{err}
	}

	// Get the TestGroup. The root dir won't be set yet, but that's OK.  We'll
	// change it after the build
	tg, errs := target.Instance(env)
	if len(errs) > 0 {
		// this should work unless the server is broken
		env.Log.Printf("Errors loading target: %v\n", errs)
		return nil, []error{errors.New("Errors loading target on the server")}
	}

	id := uuid.NewV4().String()

	s := &Submission{
		ID:                     id,
		Repository:             request.Repository,
		CommitID:               request.CommitID,
		CommitRef:              request.CommitRef,
		ClientVersion:          request.ClientVersion.String(),
		EstimatedScore:         uint(0),
		TargetID:               target.ID,
		TargetName:             target.Name,
		TargetVersion:          target.Version,
		SubmittedTargetID:      target.ID,
		SubmittedTargetName:    target.Name,
		SubmittedTargetVersion: target.Version,
		PointsAvailable:        target.Points,
		TargetType:             target.Type,
		IsMetaTarget:           target.IsMetaTarget,

		Status:      SUBMISSION_SUBMITTED,
		Score:       uint(0),
		Performance: float64(0.0),
		TestIDs:     []string{buildTest.ID},
		Errors:      []string{},

		SubmissionTime: time.Now(),

		Env:       env,
		BuildTest: buildTest,
		Tests:     tg,

		origTarget:       target,
		OrigSubmissionID: id,
		SubSubmissionIDs: []string{},
		subSubmissions:   make(map[string]*Submission),

		estimatedScores: request.EstimatedScores,
	}

	// If this is a subtarget, change the details and "submit to the metatarget".
	if target.metaTarget != nil && !target.IsMetaTarget {
		s.TargetID = target.metaTarget.ID
		s.TargetName = target.metaTarget.Name
		s.TargetVersion = target.metaTarget.Version
		s.PointsAvailable = target.metaTarget.Points
		s.IsMetaTarget = true
	}

	// This needs to come after the target name is adjusted for metatargets.
	if est, ok := request.EstimatedScores[s.TargetName]; ok {
		s.EstimatedScore = est
	}

	// We need the students to later update the students collection.  But,
	// the submission only cares about user email addresses.
	s.students = students
	s.Users = make([]string, 0, len(request.Users))
	for _, u := range request.Users {
		s.Users = append(s.Users, u.Email)
	}

	// Also, we record whether the students were staff or not so we can
	// easily filter out staff submissions. The validate step checks that
	// students and staff are not working together
	if len(students) > 0 {
		// This has already been set during validate.
		s.IsStaff, _ = students[0].IsStaff(env)
	}

	// Try and lock students now so we don't allow multiple submissions.
	// This enforces NewSubmission() can only return successfully if none
	// of the students has a pending submission. We need to do this
	// before we persist the submission.
	userLock.Lock()
	defer userLock.Unlock()

	// First pass - just check
	for _, student := range students {
		if running := pendingSubmissions[student.Email]; running {
			msg := fmt.Sprintf("Cannot submit at this time: User %v has a submission pending.", student.Email)
			env.Log.Println(msg)
			return nil, []error{errors.New(msg)}
		}
	}

	// Now lock
	for _, student := range students {
		pendingSubmissions[student.Email] = true
	}

	if env.Persistence != nil {
		if buildTest != nil {
			// If we get an error here, we can still hopefully recover. Though,
			// build updates won't be seen by the user.
			env.notifyAndLogErr("Create Build Test", buildTest, MSG_PERSIST_CREATE, 0)
		}
		// This we can't recover from
		err = env.Persistence.Notify(s, MSG_PERSIST_CREATE, 0)
	}

	// Unlock so they can resubmit
	if err != nil {
		for _, student := range students {
			delete(pendingSubmissions, student.Email)
		}
		return nil, []error{err}
	}

	return s, nil
}

func (s *Submission) TargetStats() (result *TargetStats) {
	result = &TargetStats{
		TargetName:    s.TargetName,
		TargetVersion: s.TargetVersion,
		TargetType:    s.TargetType,
		MaxScore:      s.PointsAvailable,
	}
	return
}

// Are the submission results valid, from the perspective of updating statistics?
// We only count submissions that complete successfully for assignments.
// For perf, the score has to be perfect also.
func (s *Submission) validResult() bool {
	if s.Status == SUBMISSION_COMPLETED &&
		(s.TargetType == TARGET_ASST || s.Score == s.PointsAvailable) {
		return true
	} else {
		return false
	}
}

func (student *Student) getStat(targetName string) *TargetStats {
	for _, stat := range student.Stats {
		if stat.TargetName == targetName {
			return stat
		}
	}
	return nil
}

func (student *Student) updateStats(submission *Submission) {

	// This might be nil coming out of Mongo
	if student.Stats == nil {
		student.Stats = make([]*TargetStats, 0)
	}

	// Only update the student submission count for the original submission.
	// This way, we won't increase it 4 times when they submit ASST3.
	if submission.ID == submission.OrigSubmissionID {
		student.TotalSubmissions += 1
	}

	// Find the TargetStats to update, or create a new one
	stat := student.getStat(submission.TargetName)
	if stat == nil {
		stat = submission.TargetStats()
		student.Stats = append(student.Stats, stat)
		// Sort this so it looks right coming out of mongo
		sort.Sort(StatsByName(student.Stats))
	}

	// Always increment submission count, but everything else depends on the
	// submission result
	stat.TotalSubmissions += 1

	// If the target changed, like in ASST3 where we're incrementally building it,
	// update the max score so the front-end displays it correctly.
	// TODO: We might want to keep multiple version in the stats collection, but
	// that would require aggregation (slow) on the front-end.
	if stat.TargetVersion < submission.TargetVersion {
		stat.MaxScore = submission.PointsAvailable
		stat.TargetVersion = submission.TargetVersion
	}

	if submission.validResult() {

		if stat.TargetType == TARGET_ASST {
			// High score
			if stat.HighScore < submission.Score {
				stat.HighScore = submission.Score
				stat.BestSubmission = submission.ID
			}

			// Low score
			if stat.LowScore == 0 || stat.LowScore > submission.Score {
				stat.LowScore = submission.Score
			}

			// Average
			prevTotal := float64(stat.TotalComplete) * stat.AvgScore
			stat.TotalComplete += 1
			if stat.TotalComplete == 0 {
				stat.TotalComplete = 1
				prevTotal = 0
			}
			stat.AvgScore = (prevTotal + float64(submission.Score)) / float64(stat.TotalComplete)

		} else if stat.TargetType == TARGET_PERF {
			// Best Perf
			if submission.Performance < stat.BestPerf || stat.BestPerf == 0.0 {
				stat.BestPerf = submission.Performance
				stat.BestSubmission = submission.ID
			}

			// Worst Perf
			if stat.WorstPerf < submission.Performance {
				stat.WorstPerf = submission.Performance
			}

			// Average perf
			prevPerfTotal := float64(stat.TotalComplete) * stat.AvgPerf
			stat.TotalComplete += 1
			if stat.TotalComplete == 0 {
				stat.TotalComplete = 1
				prevPerfTotal = 0.0
			}
			stat.AvgPerf = (prevPerfTotal + submission.Performance) / float64(stat.TotalComplete)
		}
	}
}

func (student *Student) IsStaff(env *TestEnvironment) (bool, error) {
	const staff = "services.auth0.user_metadata.staff"
	const email = "services.auth0.email"

	if student.isStaff == 0 {
		if env.Persistence != nil && env.Persistence.CanRetrieve() {
			who := map[string]interface{}{
				email: student.Email,
				staff: true,
			}
			filter := map[string]interface{}{staff: 1}
			res := make([]interface{}, 0)
			if err := env.Persistence.Retrieve(PERSIST_TYPE_USERS, who, filter, &res); err != nil {
				return false, err
			} else {
				// Set flag
				if len(res) > 0 {
					student.isStaff = 2
				} else {
					student.isStaff = 1
				}
			}
		} else {
			return false, errors.New("Unable to detect if student is staff")
		}
	}

	return student.isStaff == 2, nil
}

// Update students.  We copy metadata to make this quick and store the
// submision id to look up the full details.
func (s *Submission) updateStudents() {

	for _, student := range s.students {

		// Update stats
		student.updateStats(s)

		if s.Env.Persistence != nil {
			if err := s.Env.Persistence.Notify(student, MSG_PERSIST_UPDATE, 0); err != nil {
				if sbytes, jerr := json.Marshal(student); jerr != nil {
					s.Env.Log.Printf("Error updating student: %v  (%v)\n", student.Email, err)
				} else {
					s.Env.Log.Printf("Error updating student: %v  (%v)\n", string(sbytes), err)
				}
			}
		}
	}
}

func (s *Submission) unlockStudents() {
	userLock.Lock()
	defer userLock.Unlock()

	// Unblock the students from resubmitting
	for _, student := range s.students {
		delete(pendingSubmissions, student.Email)
	}
}

func (s *Submission) finish() {

	s.CompletionTime = time.Now()
	if s.Status == SUBMISSION_RUNNING {
		s.Status = SUBMISSION_COMPLETED
	}

	// Send the final submission update to the db
	s.Env.notifyAndLogErr("Finish Submission", s, MSG_PERSIST_COMPLETE, 0)

	if len(s.students) > 0 {
		s.updateStudents()
	}
}

// Clone the submission and update its details for the given target.
// This requires us to copy the existing object and modify its tests list
// and points to only include those tests that were requried for this
// particular target.
func (s *Submission) cloneAndUpdate(target *Target) *Submission {
	var copy Submission = *s

	copy.ID = uuid.NewV4().String()

	// Target details
	copy.TargetID = target.ID
	copy.TargetName = target.Name
	copy.TargetVersion = target.Version
	copy.IsMetaTarget = false
	copy.PointsAvailable = target.Points

	// Results/tests
	copy.Score = uint(0)
	copy.EstimatedScore = uint(0)
	copy.TestIDs = make([]string, 0)

	copy.SubSubmissionIDs = make([]string, 0)
	copy.subSubmissions = make(map[string]*Submission)

	// Add build test
	copy.TestIDs = append(copy.TestIDs, s.TestIDs[0])

	// Find all tests that were needed for this sub-part
	for _, test := range s.Tests.Tests {
		if test.requiredBy[target.Name] {
			copy.TestIDs = append(copy.TestIDs, test.ID)
		}
	}

	if est, ok := s.estimatedScores[target.Name]; ok {
		copy.EstimatedScore = est
	}

	return &copy
}

// Split the submission into multiple submissions based on the metatarget.
func (s *Submission) split() []*Submission {
	// For single target assignments, we skip this step.
	if !s.IsMetaTarget {
		return nil
	}

	// We need to create a submission for the original target, as well as
	// all previous subtargets.
	submissions := make([]*Submission, 0)

	// Two cases:
	//  1) The original target was a a metatarget. In this case, all of the targets
	//     are stored in the previousSubtargets.
	//  2) We upgraded to a metatarget. In that case, we need to add all of the
	//     previous subtargets AND the original target.

	if !s.origTarget.IsMetaTarget {
		submissions = append(submissions, s.cloneAndUpdate(s.origTarget))
	}

	for _, target := range s.origTarget.previousSubTargets {
		submissions = append(submissions, s.cloneAndUpdate(target))
	}

	return submissions
}

func (s *Submission) abort() {
	s.Status = SUBMISSION_ABORTED
	s.Score = 0
	s.Performance = float64(0)
}

func (s *Submission) updateScore(test *Test) {
	s.Score += test.PointsEarned
	s.Env.Persistence.Notify(s, MSG_PERSIST_UPDATE, MSG_FIELD_SCORE)
}

// Synchronous submission runner
func (s *Submission) Run() error {
	// Run the build first.  Right now this is the only thing the front-end sees.
	// We'll add the rest of the tests if this passes, otherwise we don't waste the
	// disk space.

	var err error

	// So we know it's not nil
	if s.Env.Persistence == nil {
		s.Env.Persistence = &DoNothingPersistence{}
	}

	defer s.unlockStudents()
	defer s.finish()

	// Build os161
	if s.BuildTest != nil {
		s.Status = SUBMISSION_BUILDING
		s.Env.notifyAndLogErr("Submission Status Building", s, MSG_PERSIST_UPDATE, MSG_FIELD_STATUS)
		s.BuildTest.SubmissionID = s.ID

		res, err := s.BuildTest.Run(s.Env)
		if err != nil {
			s.Status = SUBMISSION_ABORTED
			s.Env.notifyAndLogErr("Submission Complete (Aborted)", s, MSG_PERSIST_COMPLETE, 0)
			s.Errors = append(s.Errors, fmt.Sprintf("%v", err))
			return err
		}

		// Build output
		s.Env.RootDir = res.RootDir

		// Clean up temp build directory
		if len(res.TempDir) > 0 {
			defer os.RemoveAll(res.TempDir)
		}

		s.OverlayCommitID = s.BuildTest.overlayCommitID
	}

	// Build succeeded, update things accordingly
	for _, test := range s.Tests.Tests {
		// Link the test to this submission.
		test.SubmissionID = s.ID

		// Add test IDs to DB
		s.TestIDs = append(s.TestIDs, test.ID)

		// Create the test object in the DB
		// If this fails, we abort the submission beacase we can't verify the results
		err = s.Env.Persistence.Notify(test, MSG_PERSIST_CREATE, 0)
		if err != nil {
			s.Status = SUBMISSION_ABORTED
			s.Errors = append(s.Errors, fmt.Sprintf("%v", err))
			return err
		}
	}

	// Run it
	s.Status = SUBMISSION_RUNNING
	s.Env.notifyAndLogErr("Submission Status (Running) ", s, MSG_PERSIST_UPDATE, MSG_FIELD_TESTS|MSG_FIELD_STATUS)

	runner := NewDependencyRunner(s.Tests)
	done := runner.Run()

	// Split up the target into multiple sub-targets. If splits is non-empty,
	// we are now running the metatarget up to and including the original target.
	// We want to do this *before* running the target so that the front-end can
	// display each submission as running simultaneously.
	splits := s.split()
	for _, newSubmission := range splits {
		s.Env.notifyAndLogErr("Create Split Submission", newSubmission, MSG_PERSIST_CREATE, 0)
		defer newSubmission.finish()
		s.SubSubmissionIDs = append(s.SubSubmissionIDs, newSubmission.ID)
		s.subSubmissions[newSubmission.TargetName] = newSubmission
	}

	// Update the score unless a test aborts, then it's 0 and we abort (eventually)
	for r := range done {
		if s.Status == SUBMISSION_RUNNING {
			if r.Test.Result == TEST_RESULT_ABORT {
				s.abort()
				for _, other := range splits {
					// Abort all
					other.abort()
				}
			} else {
				// Always update the metasubmission, and possibly subtarget submissions.
				s.updateScore(r.Test)
				for _, other := range splits {
					if r.Test.TargetName == other.TargetName {
						other.updateScore(r.Test)
					}
				}
			}

		}
		if r.Err != nil {
			s.Errors = append(s.Errors, fmt.Sprintf("%v", r.Err))
		}
	}

	return err
}

// On success, KeyGen returns the public key of the newly generated public/private key pair
func KeyGen(email, token string, env *TestEnvironment) (string, error) {

	if len(env.KeyDir) == 0 {
		return "", errors.New("No key directory specified")
	} else if _, err := os.Stat(env.KeyDir); err != nil {
		return "", errors.New("Key directory not found")
	}

	// Find user
	students, err := getStudents(email, token, env)
	if err != nil {
		return "", err
	}

	studentDir := path.Join(env.KeyDir, email)
	privkey := path.Join(studentDir, "id_rsa")
	pubkey := privkey + ".pub"

	if _, err = os.Stat(studentDir); err == nil {
		os.Remove(privkey)
		os.Remove(pubkey)
	} else {
		err = os.Mkdir(studentDir, 0770)
		if err != nil {
			return "", err
		}
	}

	// Generate key
	cmd := exec.Command("ssh-keygen", "-C", "test161@ops-class.org", "-N", "", "-f", privkey)
	cmd.Dir = env.KeyDir
	err = cmd.Run()
	if err != nil {
		return "", err
	}

	data, err := ioutil.ReadFile(pubkey)
	if err != nil {
		return "", err
	}

	keytext := string(data)

	// Update user
	students[0].PublicKey = keytext
	if env.Persistence != nil {
		err = env.Persistence.Notify(students[0], MSG_PERSIST_UPDATE, 0)
	}

	return keytext, nil
}

// RequestKeyResonse is the repsonse we send back during validation if the keys
// aren't up-to-date.
type RequestKeyResonse struct {
	User string
	Key  string
}

// Check if the local copy of the key is up-to-date.
// Return an empty key if the user's key has not been created, or the
// new key if the hash is different.
func (req *SubmissionRequest) CheckUserKeys(env *TestEnvironment) []*RequestKeyResonse {

	res := []*RequestKeyResonse{}

	for _, user := range req.Users {
		studentDir := path.Join(env.KeyDir, user.Email)
		privkey := path.Join(studentDir, "id_rsa")
		if _, err := os.Stat(privkey); err != nil {
			// No key, inform
			res = append(res, &RequestKeyResonse{
				User: user.Email,
				Key:  "",
			})
			continue
		}

		// Get hash
		data, err := ioutil.ReadFile(privkey)
		if err != nil {
			env.Log.Printf("Error reading private key (%v): %v", privkey, err)
			continue
		}

		raw := md5.Sum(data)
		hash := strings.ToLower(hex.EncodeToString(raw[:]))

		if hash != user.KeyHash {
			res = append(res, &RequestKeyResonse{
				User: user.Email,
				Key:  string(data),
			})
		}
	}

	return res
}
