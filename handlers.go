package main

import (
    "bytes"
    "errors"
    "time"
    "fmt"
    "strconv"
    "html/template"
    "net/http"
    "unicode/utf8"

    "golang.org/x/crypto/bcrypt"
    "github.com/gorilla/mux"
    "github.com/gorilla/context"
    "github.com/justinas/nosurf"
    "github.com/ubccr/goipa"
    "github.com/ubccr/mokey/model"
    "github.com/Sirupsen/logrus"
    "github.com/spf13/viper"
)

func renderTemplate(w http.ResponseWriter, t *template.Template, data interface{}) {
    var buf bytes.Buffer
    err := t.ExecuteTemplate(&buf, "layout", data)

    if err != nil {
        logrus.Printf("Error rendering template: %s", err)
        http.Error(w, "Fatal error rendering template", http.StatusInternalServerError)
        return
    }

    buf.WriteTo(w)
}

func errorHandler(app *Application, w http.ResponseWriter, status int) {
    w.WriteHeader(status)

    renderTemplate(w, app.templates["error.html"], nil)
}

func setupAccount(app *Application, questions []*model.SecurityQuestion, token *model.Token, r *http.Request) (error) {
    qid := r.FormValue("qid")
    answer := r.FormValue("answer")
    pass := r.FormValue("password")
    pass2 := r.FormValue("password2")

    if len(pass) < 8 || len(pass2) < 8 {
        return errors.New("Please set a password at least 8 characters in length.")
    }

    if pass != pass2 {
        return errors.New("Password do not match. Please confirm your password.")
    }

    if len(qid) == 0 || len(answer) == 0 {
        return errors.New("Please choose a security question and answer.")
    }

    if utf8.RuneCountInString(answer) < 2 || utf8.RuneCountInString(answer) > 100 {
        return errors.New("Invalid answer. Must be between 2 and 100 characters long.")
    }

    q, err := strconv.Atoi(qid)
    if err != nil {
        return errors.New("Invalid security question")
    }

    found := false
    for _, sq := range questions {
        if sq.Id == q {
            found = true
            break
        }
    }

    if found == false {
        return errors.New("Invalid security question")
    }

    hash, err := bcrypt.GenerateFromPassword([]byte(answer), bcrypt.DefaultCost)
    if err != nil {
        logrus.WithFields(logrus.Fields{
            "uid": token.UserName,
            "error": err.Error(),
        }).Error("failed to generate bcrypt hash of answer")
        return errors.New("Fatal system error. Please contact ccr-help.")
    }

    // Setup password in FreeIPA
    err = setPassword(token.UserName, "", pass)
    if err != nil {
        if ierr, ok := err.(*ipa.ErrPasswordPolicy); ok {
            logrus.WithFields(logrus.Fields{
                "uid": token.UserName,
                "error": ierr.Error(),
            }).Error("password does not conform to policy")
            return errors.New("Your password is too weak. Please ensure your password includes a number and lower/upper case character")
        }

        if ierr, ok :=  err.(*ipa.ErrInvalidPassword); ok {
            logrus.WithFields(logrus.Fields{
                "uid": token.UserName,
                "error": ierr.Error(),
            }).Error("invalid password from FreeIPA")
            return errors.New("Invalid password.")
        }

        logrus.WithFields(logrus.Fields{
            "uid": token.UserName,
            "error": err.Error(),
        }).Error("failed to set user password in FreeIPA")
        return errors.New("Fatal system error. Please contact ccr-help.")
    }


    // Save security answer
    a := &model.SecurityAnswer{
        UserName: token.UserName,
        QuestionId: q,
        Answer: string(hash), }

    err = model.StoreAnswer(app.db, a)
    if err != nil {
        logrus.WithFields(logrus.Fields{
            "uid": token.UserName,
            "error": err.Error(),
        }).Error("failed to save answer to the database")
        return errors.New("Fatal system error. Please contact ccr-help.")
    }


    // Destroy token
    err = model.DestroyToken(app.db, token.Token)
    if err != nil {
        logrus.WithFields(logrus.Fields{
            "uid": token.UserName,
            "error": err.Error(),
        }).Error("failed to remove token from database")
        return errors.New("Fatal system error. Please contact ccr-help.")
    }

    return nil
}

func resetPassword(app *Application, answer *model.SecurityAnswer, token *model.Token, r *http.Request) (error) {
    ans := r.FormValue("answer")
    pass := r.FormValue("password")
    pass2 := r.FormValue("password2")

    if len(pass) < 8 || len(pass2) < 8 {
        return errors.New("Please set a password at least 8 characters in length.")
    }

    if pass != pass2 {
        return errors.New("Password do not match. Please confirm your password.")
    }

    if utf8.RuneCountInString(ans) < 2 || utf8.RuneCountInString(ans) > 100 {
        return errors.New("Invalid answer. Must be between 2 and 100 characters long.")
    }

    err := bcrypt.CompareHashAndPassword([]byte(answer.Answer), []byte(ans))
    if err != nil {
        return errors.New("The security answer you provided does not match. Please check that you are entering the correct answer.")
    }

    // Setup password in FreeIPA
    err = setPassword(token.UserName, "", pass)
    if err != nil {
        if ierr, ok := err.(*ipa.ErrPasswordPolicy); ok {
            logrus.WithFields(logrus.Fields{
                "uid": token.UserName,
                "error": ierr.Error(),
            }).Error("password does not conform to policy")
            return errors.New("Your password is too weak. Please ensure your password includes a number and lower/upper case character")
        }

        if ierr, ok :=  err.(*ipa.ErrInvalidPassword); ok {
            logrus.WithFields(logrus.Fields{
                "uid": token.UserName,
                "error": ierr.Error(),
            }).Error("invalid password from FreeIPA")
            return errors.New("Invalid password.")
        }

        logrus.WithFields(logrus.Fields{
            "uid": token.UserName,
            "error": err.Error(),
        }).Error("failed to set user password in FreeIPA")
        return errors.New("Fatal system error. Please contact ccr-help.")
    }

    // Destroy token
    err = model.DestroyToken(app.db, token.Token)
    if err != nil {
        logrus.WithFields(logrus.Fields{
            "uid": token.UserName,
            "error": err.Error(),
        }).Error("failed to remove token from database")
        return errors.New("Fatal system error. Please contact ccr-help.")
    }

    return nil
}

func SetupAccountHandler(app *Application) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        token, err := model.FetchToken(app.db, mux.Vars(r)["token"], viper.GetInt("setup_max_age"))
        if err != nil {
            logrus.WithFields(logrus.Fields{
                "error": err.Error(),
            }).Error("Failed to fetch token from database")
            w.WriteHeader(http.StatusNotFound)
            renderTemplate(w, app.templates["404.html"], nil)
            return
        }

        if token.Attempts > viper.GetInt("max_attempts") {
            logrus.WithFields(logrus.Fields{
                "token": token.Token,
                "uid": token.UserName,
            }).Error("Too many attempts for token.")
            w.WriteHeader(http.StatusNotFound)
            renderTemplate(w, app.templates["404.html"], nil)
            return
        }

        questions, err := model.FetchQuestions(app.db)
        if err != nil {
            logrus.WithFields(logrus.Fields{
                "error": err.Error(),
            }).Error("Failed to fetch questions from database")
            errorHandler(app, w, http.StatusInternalServerError)
            return
        }

        message := ""
        completed := false

        if r.Method == "POST" {
            err := setupAccount(app, questions, token, r)
            if err != nil {
                message = err.Error()
                completed = false

                err := model.IncrementToken(app.db, token.Token)
                if err != nil {
                    logrus.WithFields(logrus.Fields{
                        "error": err.Error(),
                    }).Error("Failed to increment token attempts")
                }
            } else {
                completed = true
                err = app.SendEmail(token.Email, fmt.Sprintf("[%s] Your account confirmation", viper.GetString("email_prefix")), "setup-account-confirm.txt", time.Now())
                if err != nil {
                    logrus.WithFields(logrus.Fields{
                        "uid": token.UserName,
                        "error": err,
                    }).Error("failed to send setup confirmation email to user")
                }
            }
        }

        vars := map[string]interface{}{
                "token": nosurf.Token(r),
                "uid": token.UserName,
                "completed": completed,
                "questions": questions,
                "message": message}

        renderTemplate(w, app.templates["setup-account.html"], vars)
    })
}

func ResetPasswordHandler(app *Application) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        token, err := model.FetchToken(app.db, mux.Vars(r)["token"], viper.GetInt("reset_max_age"))
        if err != nil {
            logrus.WithFields(logrus.Fields{
                "error": err.Error(),
            }).Error("Failed to fetch token from database")
            w.WriteHeader(http.StatusNotFound)
            renderTemplate(w, app.templates["404.html"], nil)
            return
        }

        if token.Attempts > viper.GetInt("max_attempts") {
            logrus.WithFields(logrus.Fields{
                "token": token.Token,
                "uid": token.UserName,
            }).Error("Too many attempts for token.")
            w.WriteHeader(http.StatusNotFound)
            renderTemplate(w, app.templates["404.html"], nil)
            return
        }

        answer, err := model.FetchAnswer(app.db, token.UserName)
        if err != nil {
            logrus.WithFields(logrus.Fields{
                "uid": token.UserName,
                "error": err,
            }).Error("Failed to fetch security answer")
            w.WriteHeader(http.StatusNotFound)
            renderTemplate(w, app.templates["404.html"], nil)
            return
        }

        message := ""
        completed := false

        if r.Method == "POST" {
            err := resetPassword(app, answer, token, r)
            if err != nil {
                message = err.Error()
                completed = false

                err := model.IncrementToken(app.db, token.Token)
                if err != nil {
                    logrus.WithFields(logrus.Fields{
                        "error": err.Error(),
                    }).Error("Failed to increment token attempts")
                }
            } else {
                completed = true
                err = app.SendEmail(token.Email, fmt.Sprintf("[%s] Your password change confirmation", viper.GetString("email_prefix")), "reset-password-confirm.txt", time.Now())
                if err != nil {
                    logrus.WithFields(logrus.Fields{
                        "uid": token.UserName,
                        "error": err,
                    }).Error("failed to send reset confirmation email to user")
                }
            }
        }

        vars := map[string]interface{}{
                "token": nosurf.Token(r),
                "uid": token.UserName,
                "completed": completed,
                "question": answer.Question,
                "message": message}

        renderTemplate(w, app.templates["reset-password.html"], vars)
    })
}

func setPassword(uid, oldPass, newPass string) (error) {
    c := NewIpaClient(true)

    if len(oldPass) == 0 {
        rand, err := c.ResetPassword(uid)
        if err != nil {
            return  err
        }
        oldPass = rand
    }

    err := c.ChangePassword(uid, oldPass, newPass)
    if err != nil {
        return err
    }

    return nil
}

func tryAuth(uid, pass string) (string, *ipa.UserRecord, error) {
    if len(uid) == 0 || len(pass) == 0 {
        return "", nil, errors.New("Please provide a uid/password")
    }

    c := NewIpaClient(true)

    sess, err := c.Login(uid, pass)
    if err != nil {
        logrus.WithFields(logrus.Fields{
            "uid": uid,
            "ipa_client_error": err,
        }).Error("tryauth: failed login attempt")
        return "", nil, errors.New("Invalid login")
    }

    userRec, err := c.UserShow(uid)
    if err != nil {
        logrus.WithFields(logrus.Fields{
            "uid": uid,
            "ipa_client_error": err,
        }).Error("tryauth: failed to fetch user info")
        return "", nil, errors.New("Invalid login")
    }

    return sess, userRec, nil
}

func LoginHandler(app *Application) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        message := ""

        if r.Method == "POST" {
            uid := r.FormValue("uid")
            pass := r.FormValue("password")

            sid, userRec, err := tryAuth(uid, pass)
            if err != nil {
                message = err.Error()
            } else {
                session, _ := app.cookieStore.Get(r, MOKEY_COOKIE_SESSION)
                session.Values[MOKEY_COOKIE_SID] = sid
                session.Values[MOKEY_COOKIE_USER] = userRec
                err = session.Save(r, w)
                if err != nil {
                    logrus.WithFields(logrus.Fields{
                        "error": err.Error(),
                    }).Error("loginhandler: failed to save session")
                    errorHandler(app, w, http.StatusInternalServerError)
                    return
                }

                http.Redirect(w, r, "/", 302)
                return
            }
        }

        vars := map[string]interface{}{
                "token": nosurf.Token(r),
                "message": message}

        renderTemplate(w, app.templates["login.html"], vars)
    })
}

func LogoutHandler(app *Application) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        session, _ := app.cookieStore.Get(r, MOKEY_COOKIE_SESSION)
        delete(session.Values, MOKEY_COOKIE_SID)
        delete(session.Values, MOKEY_COOKIE_USER)
        session.Options.MaxAge = -1

        err := session.Save(r, w)
        if err != nil {
            logrus.WithFields(logrus.Fields{
                "error": err.Error(),
            }).Error("logouthandler: failed to save session")
            errorHandler(app, w, http.StatusInternalServerError)
            return
        }

        http.Redirect(w, r, "/auth/login", 302)
    })
}

func IndexHandler(app *Application) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        user := context.Get(r, "user").(*ipa.UserRecord)
        if user == nil {
            logrus.Error("index handler: user not found in request context")
            errorHandler(app, w, http.StatusInternalServerError)
            return
        }

        vars := map[string]interface{}{
                "user": user}

        renderTemplate(w, app.templates["index.html"], vars)
    })
}
