package imap

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"mime"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dustin/go-humanize"
	"github.com/jhillyerd/enmime"

	"golang.org/x/net/html/charset"
)

// AddSlashes adds slashes to double quotes
var AddSlashes = strings.NewReplacer(`"`, `\"`)

// RemoveSlashes removes slashes before double quotes
var RemoveSlashes = strings.NewReplacer(`\"`, `"`)

// Dialer is basically an IMAP connection
type Dialer struct {
	conn      *tls.Conn
	Folder    string
	Username  string
	Password  string
	Host      string
	Port      int
	strtokI   int
	strtok    string
	connected bool
	Logger    *log.Logger
}

// EmailAddresses are a map of email address to names
type EmailAddresses map[string]string

// Email is an email message
type Email struct {
	Flags       []string
	Received    time.Time
	Sent        time.Time
	Size        uint64
	Subject     string
	UID         int
	MessageID   string
	From        EmailAddresses
	To          EmailAddresses
	ReplyTo     EmailAddresses
	CC          EmailAddresses
	BCC         EmailAddresses
	Text        string
	HTML        string
	Attachments []Attachment
}

// Attachment is an Email attachment
type Attachment struct {
	Name     string
	MimeType string
	Content  []byte
}

func (e EmailAddresses) String() string {
	emails := strings.Builder{}
	i := 0
	for e, n := range e {
		if i != 0 {
			emails.WriteString(", ")
		}
		if len(n) != 0 {
			if strings.ContainsRune(n, ',') {
				emails.WriteString(fmt.Sprintf(`"%s" <%s>`, AddSlashes.Replace(n), e))
			} else {
				emails.WriteString(fmt.Sprintf(`%s <%s>`, n, e))
			}
		} else {
			emails.WriteString(fmt.Sprintf("%s", e))
		}
		i++
	}
	return emails.String()
}

func (e Email) String() string {
	email := strings.Builder{}

	email.WriteString(fmt.Sprintf("Subject: %s\n", e.Subject))

	if len(e.To) != 0 {
		email.WriteString(fmt.Sprintf("To: %s\n", e.To))
	}
	if len(e.From) != 0 {
		email.WriteString(fmt.Sprintf("From: %s\n", e.From))
	}
	if len(e.CC) != 0 {
		email.WriteString(fmt.Sprintf("CC: %s\n", e.CC))
	}
	if len(e.BCC) != 0 {
		email.WriteString(fmt.Sprintf("BCC: %s\n", e.BCC))
	}
	if len(e.ReplyTo) != 0 {
		email.WriteString(fmt.Sprintf("ReplyTo: %s\n", e.ReplyTo))
	}
	if len(e.Text) != 0 {
		if len(e.Text) > 20 {
			email.WriteString(fmt.Sprintf("Text: %s...", e.Text[:20]))
		} else {
			email.WriteString(fmt.Sprintf("Text: %s", e.Text))
		}
		email.WriteString(fmt.Sprintf("(%s)\n", humanize.Bytes(uint64(len(e.Text)))))
	}
	if len(e.HTML) != 0 {
		if len(e.HTML) > 20 {
			email.WriteString(fmt.Sprintf("HTML: %s...", e.HTML[:20]))
		} else {
			email.WriteString(fmt.Sprintf("HTML: %s", e.HTML))
		}
		email.WriteString(fmt.Sprintf(" (%s)\n", humanize.Bytes(uint64(len(e.HTML)))))
	}

	if len(e.Attachments) != 0 {
		email.WriteString(fmt.Sprintf("%d Attachment(s): %s\n", len(e.Attachments), e.Attachments))
	}

	return email.String()
}

func (a Attachment) String() string {
	return fmt.Sprintf("%s (%s %s)", a.Name, a.MimeType, humanize.Bytes(uint64(len(a.Content))))
}

func New(username string, password string, host string, port int) *Dialer {
	return &Dialer{
		Username: username,
		Password: password,
		Host:     host,
		Port:     port,
	}
}

func (d *Dialer) Connect() error {
	d.log("", "establishing connection")

	conn, err := tls.Dial("tcp", d.Host+":"+strconv.Itoa(d.Port), nil)
	if err != nil {
		d.log("", fmt.Sprintf("failed to connect: %s", err))
		return err
	}
	d.conn = conn
	d.connected = true

	return d.Login(d.Username, d.Password)
}

func (d *Dialer) log(folder string, msg interface{}) {
	if d.Logger != nil {
		d.Logger.Println(msg)
	}
}

// Close closes the imap connection
func (d *Dialer) Close() (err error) {
	if d.connected {
		d.log(d.Folder, "closing connection")
		err = d.conn.Close()
		if err != nil {
			return fmt.Errorf("imap close: %s", err)
		}
		d.connected = false
	}
	return
}

const nl = "\r\n"

func dropNl(b []byte) []byte {
	if len(b) >= 1 && b[len(b)-1] == '\n' {
		if len(b) >= 2 && b[len(b)-2] == '\r' {
			return b[:len(b)-2]
		} else {
			return b[:len(b)-1]
		}
	}
	return b
}

var atom = regexp.MustCompile(`{\d+}$`)

// Exec executes the command on the imap connection
func (d *Dialer) Exec(command string, buildResponse bool, processLine func(line []byte) error) (response string, err error) {
	var resp strings.Builder
	tag := []byte(fmt.Sprintf("%X", bid2()))

	c := fmt.Sprintf("%s %s\r\n", tag, command)

	d.log(d.Folder, strings.Replace(fmt.Sprintf("%s %s", "->", strings.TrimSpace(c)), fmt.Sprintf(`"%s"`, d.Password), `"****"`, -1))

	_, err = d.conn.Write([]byte(c))
	if err != nil {
		return
	}

	r := bufio.NewReader(d.conn)

	if buildResponse {
		resp = strings.Builder{}
	}
	var line []byte
	for err == nil {
		line, err = r.ReadBytes('\n')
		for {
			if a := atom.Find(dropNl(line)); a != nil {
				// fmt.Printf("%s\n", a)
				var n int
				n, err = strconv.Atoi(string(a[1 : len(a)-1]))
				if err != nil {
					return
				}

				buf := make([]byte, n)
				_, err = io.ReadFull(r, buf)
				if err != nil {
					return
				}
				line = append(line, buf...)

				buf, err = r.ReadBytes('\n')
				if err != nil {
					return
				}
				line = append(line, buf...)

				continue
			}
			break
		}

		d.log(d.Folder, fmt.Sprintf("<- %s", dropNl(line)))

		if len(line) >= 19 && bytes.Equal(line[:16], tag) {
			if !bytes.Equal(line[17:19], []byte("OK")) {
				err = fmt.Errorf("imap command failed: %s", line[20:])
				return
			}
			break
		}

		if processLine != nil {
			if err = processLine(line); err != nil {
				return
			}
		}
		if buildResponse {
			resp.Write(line)
		}
	}

	if err != nil {
		return "", err
	}

	if buildResponse {
		if resp.Len() != 0 {
			return resp.String(), nil
		}
		return "", nil
	}
	return
}

// Login attempts to login
func (d *Dialer) Login(username string, password string) (err error) {
	_, err = d.Exec(fmt.Sprintf(`LOGIN "%s" "%s"`, AddSlashes.Replace(username), AddSlashes.Replace(password)), false, nil)
	return
}

// GetFolders returns all folders
func (d *Dialer) GetFolders() (folders []string, err error) {
	folders = make([]string, 0)
	_, err = d.Exec(`LIST "" "*"`, false, func(line []byte) (err error) {
		line = dropNl(line)
		if b := bytes.IndexByte(line, '\n'); b != -1 {
			folders = append(folders, string(line[b+1:]))
		} else {
			i := len(line) - 1
			quoted := line[i] == '"'
			delim := byte(' ')
			if quoted {
				delim = '"'
				i--
			}
			end := i
			for i > 0 {
				if line[i] == delim {
					if !quoted || line[i-1] != '\\' {
						break
					}
				}
				i--
			}
			folders = append(folders, RemoveSlashes.Replace(string(line[i+1:end+1])))
		}
		return
	})
	if err != nil {
		return nil, err
	}

	return folders, nil
}

// SelectFolder selects a folder
func (d *Dialer) SelectFolder(folder string) (err error) {
	_, err = d.Exec(`SELECT "`+AddSlashes.Replace(folder)+`"`, true, nil)
	if err != nil {
		return
	}
	d.Folder = folder
	return nil
}

// ExamineFolder selects a folder in read only mode
func (d *Dialer) ExamineFolder(folder string) (err error) {
	_, err = d.Exec(`EXAMINE "`+AddSlashes.Replace(folder)+`"`, true, nil)
	if err != nil {
		return
	}
	d.Folder = folder
	return nil
}

// GetUIDs returns the UIDs in the current folder that match the search
func (d *Dialer) GetUIDs(search string) (uids []int, err error) {
	uids = make([]int, 0)
	t := []byte{' ', '\r', '\n'}
	r, err := d.Exec(`UID SEARCH `+search, true, nil)
	if err != nil {
		return nil, err
	}
	if d.StrtokInit(r, t) == "*" && d.Strtok(t) == "SEARCH" {
		for {
			uid := string(d.Strtok(t))
			if len(uid) == 0 {
				break
			}
			u, err := strconv.Atoi(string(uid))
			if err != nil {
				return nil, err
			}
			uids = append(uids, u)
		}
	}

	return uids, nil
}

const (
	EDate uint8 = iota
	ESubject
	EFrom
	ESender
	EReplyTo
	ETo
	ECC
	EBCC
	EInReplyTo
	EMessageID
)

const (
	EEName uint8 = iota
	// EESR is unused and should be ignored
	EESR
	EEMailbox
	EEHost
)

// GetEmails returns email with their bodies for the given UIDs in the current folder.
// If no UIDs are given, they everything in the current folder is selected
func (d *Dialer) GetEmails(uids ...int) (emails map[int]*Email, err error) {
	emails, err = d.GetOverviews(uids...)
	if err != nil {
		return nil, err
	}

	if len(emails) == 0 {
		return
	}

	uidsStr := strings.Builder{}
	if len(uids) == 0 {
		uidsStr.WriteString("1:*")
	} else {
		i := 0
		for u := range emails {
			if u == 0 {
				continue
			}

			if i != 0 {
				uidsStr.WriteByte(',')
			}
			uidsStr.WriteString(strconv.Itoa(u))
			i++
		}
	}

	var records [][]*Token
	r, err := d.Exec("UID FETCH "+uidsStr.String()+" BODY.PEEK[]", true, nil)
	if err != nil {
		return
	}

	records, err = d.ParseFetchResponse(r)
	if err != nil {
		return
	}

	for _, tks := range records {
		e := &Email{}
		skip := 0
		success := true
		for i, t := range tks {
			if skip > 0 {
				skip--
				continue
			}
			if err = d.CheckType(t, []TType{TLiteral}, tks, "in root"); err != nil {
				return
			}
			switch t.Str {
			case "BODY[]":
				if err = d.CheckType(tks[i+1], []TType{TAtom}, tks, "after BODY[]"); err != nil {
					return
				}
				msg := tks[i+1].Str
				r := strings.NewReader(msg)

				env, err := enmime.ReadEnvelope(r)
				if err != nil {
					d.log(d.Folder, "email body could not be parsed, skipping: "+err.Error())
					success = false

					// continue RecL
				} else {

					e.Subject = env.GetHeader("Subject")
					e.Text = env.Text
					e.HTML = env.HTML

					if len(env.Attachments) != 0 {
						for _, a := range env.Attachments {
							e.Attachments = append(e.Attachments, Attachment{
								Name:     a.FileName,
								MimeType: a.ContentType,
								Content:  a.Content,
							})
						}
					}

					if len(env.Inlines) != 0 {
						for _, a := range env.Inlines {
							e.Attachments = append(e.Attachments, Attachment{
								Name:     a.FileName,
								MimeType: a.ContentType,
								Content:  a.Content,
							})
						}
					}

					for _, a := range []struct {
						dest   *EmailAddresses
						header string
					}{
						{&e.From, "From"},
						{&e.ReplyTo, "Reply-To"},
						{&e.To, "To"},
						{&e.CC, "cc"},
						{&e.BCC, "bcc"},
					} {
						alist, _ := env.AddressList(a.header)
						(*a.dest) = make(map[string]string, len(alist))
						for _, addr := range alist {
							(*a.dest)[strings.ToLower(addr.Address)] = addr.Name
						}
					}
				}
				skip++
			case "UID":
				if err = d.CheckType(tks[i+1], []TType{TNumber}, tks, "after UID"); err != nil {
					return
				}
				e.UID = tks[i+1].Num
				skip++
			}
		}

		if success {
			emails[e.UID].Subject = e.Subject
			emails[e.UID].From = e.From
			emails[e.UID].ReplyTo = e.ReplyTo
			emails[e.UID].To = e.To
			emails[e.UID].CC = e.CC
			emails[e.UID].BCC = e.BCC
			emails[e.UID].Text = e.Text
			emails[e.UID].HTML = e.HTML
			emails[e.UID].Attachments = e.Attachments
		} else {
			delete(emails, e.UID)
		}
	}
	return
}

// GetOverviews returns emails without bodies for the given UIDs in the current folder.
// If no UIDs are given, they everything in the current folder is selected
func (d *Dialer) GetOverviews(uids ...int) (emails map[int]*Email, err error) {
	uidsStr := strings.Builder{}
	if len(uids) == 0 {
		uidsStr.WriteString("1:*")
	} else {
		for i, u := range uids {
			if u == 0 {
				continue
			}

			if i != 0 {
				uidsStr.WriteByte(',')
			}
			uidsStr.WriteString(strconv.Itoa(u))
		}
	}

	var records [][]*Token
	r, err := d.Exec("UID FETCH "+uidsStr.String()+" ALL", true, nil)
	if err != nil {
		return
	}

	if len(r) == 0 {
		return
	}

	records, err = d.ParseFetchResponse(r)
	if err != nil {
		return nil, err
	}

	emails = make(map[int]*Email, len(uids))
	CharsetReader := func(label string, input io.Reader) (io.Reader, error) {
		label = strings.Replace(label, "windows-", "cp", -1)
		encoding, _ := charset.Lookup(label)
		return encoding.NewDecoder().Reader(input), nil
	}
	dec := mime.WordDecoder{CharsetReader: CharsetReader}

	// RecordsL:
	for _, tks := range records {
		e := &Email{}
		skip := 0
		for i, t := range tks {
			if skip > 0 {
				skip--
				continue
			}
			if err = d.CheckType(t, []TType{TLiteral}, tks, "in root"); err != nil {
				return nil, err
			}
			switch t.Str {
			case "FLAGS":
				if err = d.CheckType(tks[i+1], []TType{TContainer}, tks, "after FLAGS"); err != nil {
					return nil, err
				}
				e.Flags = make([]string, len(tks[i+1].Tokens))
				for i, t := range tks[i+1].Tokens {
					if err = d.CheckType(t, []TType{TLiteral}, tks, "for FLAGS[%d]", i); err != nil {
						return nil, err
					}
					e.Flags[i] = t.Str
				}
				skip++
			case "INTERNALDATE":
				if err = d.CheckType(tks[i+1], []TType{TQuoted}, tks, "after INTERNALDATE"); err != nil {
					return nil, err
				}
				e.Received, err = time.Parse(TimeFormat, tks[i+1].Str)
				if err != nil {
					return nil, err
				}
				e.Received = e.Received.UTC()
				skip++
			case "RFC822.SIZE":
				if err = d.CheckType(tks[i+1], []TType{TNumber}, tks, "after RFC822.SIZE"); err != nil {
					return nil, err
				}
				e.Size = uint64(tks[i+1].Num)
				skip++
			case "ENVELOPE":
				if err = d.CheckType(tks[i+1], []TType{TContainer}, tks, "after ENVELOPE"); err != nil {
					return nil, err
				}
				if err = d.CheckType(tks[i+1].Tokens[EDate], []TType{TQuoted, TNil}, tks, "for ENVELOPE[%d]", EDate); err != nil {
					return nil, err
				}
				if err = d.CheckType(tks[i+1].Tokens[ESubject], []TType{TQuoted, TAtom, TNil}, tks, "for ENVELOPE[%d]", ESubject); err != nil {
					return nil, err
				}

				e.Sent, _ = time.Parse("Mon, _2 Jan 2006 15:04:05 -0700", tks[i+1].Tokens[EDate].Str)
				e.Sent = e.Sent.UTC()

				e.Subject, err = dec.DecodeHeader(tks[i+1].Tokens[ESubject].Str)
				if err != nil {
					return nil, err
				}

				for _, a := range []struct {
					dest  *EmailAddresses
					pos   uint8
					debug string
				}{
					{&e.From, EFrom, "FROM"},
					{&e.ReplyTo, EReplyTo, "REPLYTO"},
					{&e.To, ETo, "TO"},
					{&e.CC, ECC, "CC"},
					{&e.BCC, EBCC, "BCC"},
				} {
					if tks[i+1].Tokens[EFrom].Type != TNil {
						if err = d.CheckType(tks[i+1].Tokens[a.pos], []TType{TNil, TContainer}, tks, "for ENVELOPE[%d]", a.pos); err != nil {
							return nil, err
						}
						*a.dest = make(map[string]string, len(tks[i+1].Tokens[EFrom].Tokens))
						for i, t := range tks[i+1].Tokens[a.pos].Tokens {
							if err = d.CheckType(t.Tokens[EEName], []TType{TQuoted, TNil}, tks, "for %s[%d][%d]", a.debug, i, EEName); err != nil {
								return nil, err
							}
							if err = d.CheckType(t.Tokens[EEMailbox], []TType{TQuoted, TNil}, tks, "for %s[%d][%d]", a.debug, i, EEMailbox); err != nil {
								return nil, err
							}
							if err = d.CheckType(t.Tokens[EEHost], []TType{TQuoted, TNil}, tks, "for %s[%d][%d]", a.debug, i, EEHost); err != nil {
								return nil, err
							}

							name, err := dec.DecodeHeader(t.Tokens[EEName].Str)
							if err != nil {
								return nil, err
							}

							// if t.Tokens[EEMailbox].Type == TNil {
							// 	if Verbose {
							// 		d.log(d.Folder, Brown("email address has no mailbox name (probably not a real email), skipping"))
							// 	}
							// 	continue RecordsL
							// }
							mailbox, err := dec.DecodeHeader(t.Tokens[EEMailbox].Str)
							if err != nil {
								return nil, err
							}

							host, err := dec.DecodeHeader(t.Tokens[EEHost].Str)
							if err != nil {
								return nil, err
							}

							(*a.dest)[strings.ToLower(mailbox+"@"+host)] = name
						}
					}
				}

				e.MessageID = tks[i+1].Tokens[EMessageID].Str

				skip++
			case "UID":
				if err = d.CheckType(tks[i+1], []TType{TNumber}, tks, "after UID"); err != nil {
					return nil, err
				}
				e.UID = tks[i+1].Num
				skip++
			}
		}

		emails[e.UID] = e
	}

	return
}

// Token is a fetch response token (e.g. a number, or a quoted section, or a container, etc.)
type Token struct {
	Type   TType
	Str    string
	Num    int
	Tokens []*Token
}

// TType is the enum type for token values
type TType uint8

const (
	// TUnset is an unset token; used by the parser
	TUnset TType = iota
	// TAtom is a string that's prefixed with `{n}`
	// where n is the number of bytes in the string
	TAtom
	// TNumber is a numeric literal
	TNumber
	// TLiteral is a literal (think string, ish, used mainly for field names, I hope)
	TLiteral
	// TQuoted is a quoted piece of text
	TQuoted
	// TNil is a nil value, nothing
	TNil
	// TContainer is a container of tokens
	TContainer
)

// TimeFormat is the Go time version of the IMAP times
const TimeFormat = "02-Jan-2006 15:04:05 -0700"

type tokenContainer *[]*Token

// ParseFetchResponse parses a response from a FETCH command into tokens
func (d *Dialer) ParseFetchResponse(r string) (records [][]*Token, err error) {
	records = make([][]*Token, 0)
	for {
		t := []byte{' ', '\r', '\n'}
		ok := false
		if string(d.StrtokInit(r, t)) == "*" {
			if _, err := strconv.Atoi(string(d.Strtok(t))); err == nil && string(d.Strtok(t)) == "FETCH" {
				ok = true
			}
		}

		if !ok {
			return nil, fmt.Errorf("Unable to parse Fetch line %#v", string(r[:d.GetStrtokI()]))
		}

		tokens := make([]*Token, 0)
		r = r[d.GetStrtokI()+1:]

		currentToken := TUnset
		tokenStart := 0
		tokenEnd := 0
		// escaped := false
		depth := 0
		container := make([]tokenContainer, 4)
		container[0] = &tokens

		pushToken := func() *Token {
			var t *Token
			switch currentToken {
			case TQuoted:
				t = &Token{
					Type: currentToken,
					Str:  RemoveSlashes.Replace(string(r[tokenStart : tokenEnd+1])),
				}
			case TLiteral:
				s := string(r[tokenStart : tokenEnd+1])
				num, err := strconv.Atoi(s)
				if err == nil {
					t = &Token{
						Type: TNumber,
						Num:  num,
					}
				} else {
					if s == "NIL" {
						t = &Token{
							Type: TNil,
						}
					} else {
						t = &Token{
							Type: TLiteral,
							Str:  s,
						}
					}
				}
			case TAtom:
				t = &Token{
					Type: currentToken,
					Str:  string(r[tokenStart : tokenEnd+1]),
				}
			case TContainer:
				t = &Token{
					Type:   currentToken,
					Tokens: make([]*Token, 0, 1),
				}
			}

			if t != nil {
				*container[depth] = append(*container[depth], t)
			}
			currentToken = TUnset

			return t
		}

		l := len(r)
		i := 0
		for i < l {
			b := r[i]

			switch currentToken {
			case TQuoted:
				switch b {
				case '"':
					tokenEnd = i - 1
					pushToken()
					goto Cont
				case '\\':
					i++
					goto Cont
				}
			case TLiteral:
				switch {
				case IsLiteral(rune(b)):
				default:
					tokenEnd = i - 1
					pushToken()
				}
			case TAtom:
				switch {
				case unicode.IsDigit(rune(b)):
				default:
					tokenEnd = i
					size, err := strconv.Atoi(string(r[tokenStart:tokenEnd]))
					if err != nil {
						return nil, err
					}
					i += len("}") + len(nl)
					tokenStart = i
					tokenEnd = tokenStart + size - 1
					i = tokenEnd
					pushToken()
				}
			}

			switch currentToken {
			case TUnset:
				switch {
				case b == '"':
					currentToken = TQuoted
					tokenStart = i + 1
				case IsLiteral(rune(b)):
					currentToken = TLiteral
					tokenStart = i
				case b == '{':
					currentToken = TAtom
					tokenStart = i + 1
				case b == '(':
					currentToken = TContainer
					t := pushToken()
					depth++
					container[depth] = &t.Tokens
				case b == ')':
					depth--
				}
			}

		Cont:
			if depth < 0 {
				break
			}
			i++
			if i >= l {
				tokenEnd = l
				pushToken()
			}
		}
		records = append(records, tokens)
		r = r[i+1+len(nl):]

		if len(r) == 0 {
			break
		}
	}

	return
}

// IsLiteral returns if the given byte is an acceptable literal character
func IsLiteral(b rune) bool {
	switch {
	case unicode.IsDigit(b),
		unicode.IsLetter(b),
		b == '\\',
		b == '.',
		b == '[',
		b == ']':
		return true
	}
	return false
}

// GetTokenName returns the name of the given token type token
func GetTokenName(tokenType TType) string {
	switch tokenType {
	case TUnset:
		return "TUnset"
	case TAtom:
		return "TAtom"
	case TNumber:
		return "TNumber"
	case TLiteral:
		return "TLiteral"
	case TQuoted:
		return "TQuoted"
	case TNil:
		return "TNil"
	case TContainer:
		return "TContainer"
	}
	return ""
}

func (t Token) String() string {
	tokenType := GetTokenName(t.Type)
	switch t.Type {
	case TUnset, TNil:
		return tokenType
	case TAtom, TQuoted:
		return fmt.Sprintf("(%s, len %d, chars %d %#v)", tokenType, len(t.Str), len([]rune(t.Str)), t.Str)
	case TNumber:
		return fmt.Sprintf("(%s %d)", tokenType, t.Num)
	case TLiteral:
		return fmt.Sprintf("(%s %s)", tokenType, t.Str)
	case TContainer:
		return fmt.Sprintf("(%s children: %s)", tokenType, t.Tokens)
	}
	return ""
}

// CheckType validates a type against a list of acceptable types,
// if the type of the token isn't in the list, an error is returned
func (d *Dialer) CheckType(token *Token, acceptableTypes []TType, tks []*Token, loc string, v ...interface{}) (err error) {
	ok := false
	for _, a := range acceptableTypes {
		if token.Type == a {
			ok = true
			break
		}
	}
	if !ok {
		types := ""
		for i, a := range acceptableTypes {
			if i != i {
				types += "|"
			}
			types += GetTokenName(a)
		}
		err = fmt.Errorf("IMAP%d:%s: expected %s token %s, got %+v in %v", d.Folder, types, fmt.Sprintf(loc, v...), token, tks)
	}

	return err
}
