package scenario

// prepare.go
// シナリオの内、prepareフェーズの処理

import (
	"context"
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"time"

	"github.com/isucon/isucon11-qualify/bench/model"

	"github.com/isucon/isucandar"
	"github.com/isucon/isucandar/agent"
	"github.com/isucon/isucandar/failure"
	"github.com/isucon/isucandar/worker"
	"github.com/isucon/isucon11-qualify/bench/logger"
	"github.com/isucon/isucon11-qualify/bench/random"
	"github.com/isucon/isucon11-qualify/bench/service"
)

func (s *Scenario) Prepare(ctx context.Context, step *isucandar.BenchmarkStep) error {
	logger.ContestantLogger.Printf("===> PREPARE")
	// keepPostingのuserTimerでctx終了させられてしまうのでprepareでも設定する

	//TODO: 他の得点源
	//TODO: 得点調整
	step.Result().Score.Set(ScoreStartBenchmark, 1000)
	step.Result().Score.Set(ScoreNormalUserInitialize, 0)
	step.Result().Score.Set(ScoreGraphExcellent, 200)
	step.Result().Score.Set(ScoreGraphGood, 150)
	step.Result().Score.Set(ScoreGraphNormal, 100)
	step.Result().Score.Set(ScoreGraphBad, 60)
	step.Result().Score.Set(ScoreGraphWorst, 10)
	step.Result().Score.Set(ScoreReadInfoCondition, 40)
	step.Result().Score.Set(ScoreReadWarningCondition, 20)
	step.Result().Score.Set(ScoreReadCriticalCondition, 10)

	//初期データの生成
	logger.AdminLogger.Println("start: load initial data")
	s.InitializeData(ctx)
	logger.AdminLogger.Println("finish: load initial data")
	s.realTimePrepareStartedAt = time.Now()

	// TODO: JIA API が立ち上がるまで待つ方法をもうちょいマシにする
	jiaWait := time.After(5 * time.Second)

	//initialize
	initializer, err := s.NewAgent(
		agent.WithNoCache(), agent.WithNoCookie(), agent.WithTimeout(s.initializeTimeout),
	)
	if err != nil {
		logger.AdminLogger.Panic(err)
	}
	initializer.Name = "benchmarker-initializer"

	initResponse, errs := initializeAction(ctx, initializer, service.PostInitializeRequest{JIAServiceURL: s.jiaServiceURL.String()})
	for _, err := range errs {
		step.AddError(err)
	}
	if len(errs) > 0 {
		//return ErrScenarioCancel
		step.AddError(failure.NewError(ErrCritical, fmt.Errorf("initializeに失敗しました")))
		return nil
	}

	s.Language = initResponse.Language

	//jia起動待ち TODO: これで本当に良いのか？
	<-jiaWait

	// prepareチェックの実行
	if err := s.prepareCheck(ctx, step); err != nil {
		return err
	}

	errors := step.Result().Errors
	hasErrors := func() bool {
		errors.Wait()
		return len(errors.All()) > 0
	}

	if hasErrors() {
		step.AddError(failure.NewError(ErrCritical, fmt.Errorf("アプリケーション互換性チェックに失敗しました")))
		return nil
	}

	s.realTimeLoadFinishedAt = time.Now().Add(s.LoadTimeout)
	return nil
}

//エンドポイント毎の単体テスト
func (s *Scenario) prepareCheck(parent context.Context, step *isucandar.BenchmarkStep) error {
	errors := step.Result().Errors
	hasErrors := func() bool {
		errors.Wait()
		return len(errors.All()) > 0
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// NewUserでauth使うのでauthだけ先にチェック
	err := s.prepareCheckAuth(ctx, step)
	if err != nil {
		return err
	}
	if hasErrors() {
		return failure.NewError(ErrCritical, fmt.Errorf("アプリケーション互換性チェックに失敗しました"))
	}

	//ユーザー作成
	guestAgent, err := s.NewAgent(agent.WithTimeout(s.prepareTimeout))
	if err != nil {
		logger.AdminLogger.Panicln(err)
	}

	// 正常系Prepare Check
	s.prepareNormal(ctx, step)
	if hasErrors() {
		return failure.NewError(ErrCritical, fmt.Errorf("アプリケーション互換性チェックに失敗しました"))
	}

	noIsuAgent, err := s.NewAgent(agent.WithTimeout(s.prepareTimeout))
	if err != nil {
		logger.AdminLogger.Panicln(err)
	}
	noIsuUser := s.NewUser(ctx, step, noIsuAgent, model.UserTypeNormal, false)

	// 初期データで生成しているisuconユーザを利用
	isuconUser := s.normalUsers[0]
	agt, err := s.NewAgent(agent.WithTimeout(s.prepareTimeout))
	if err != nil {
		logger.AdminLogger.Panicln(err)
	}
	_, errs := authAction(ctx, agt, isuconUser.UserID)
	for _, err := range errs {
		step.AddError(err)
		return nil
	}
	isuconUser.Agent = agt

	// 各エンドポイントのチェック
	s.prepareIrregularCheckPostSignout(ctx, step)
	s.prepareIrregularCheckGetMe(ctx, guestAgent, step)
	s.prepareIrregularCheckGetIsuList(ctx, noIsuUser, guestAgent, step)
	s.prepareIrregularCheckGetIsu(ctx, isuconUser, noIsuUser, guestAgent, step)
	s.prepareIrregularCheckGetIsuIcon(ctx, isuconUser, noIsuUser, guestAgent, step)
	s.prepareIrregularCheckGetIsuGraph(ctx, isuconUser, noIsuUser, guestAgent, step)
	s.prepareIrregularCheckGetIsuConditions(ctx, isuconUser, noIsuUser, guestAgent, step)

	// MEMO: postIsuConditionのprepareチェックは確率で失敗して安定しないため、prepareステップでは行わない

	// ユーザのISUが増えるので他の検証終わった後に実行
	s.prepareCheckPostIsu(ctx, isuconUser, noIsuUser, guestAgent, step)
	if hasErrors() {
		return failure.NewError(ErrCritical, fmt.Errorf("アプリケーション互換性チェックに失敗しました"))
	}

	return nil
}

func (s *Scenario) prepareNormal(ctx context.Context, step *isucandar.BenchmarkStep) {
	// endtimeのデフォルト値をとりあえずisuconユーザのisuの値にしておく
	isu := s.normalUsers[0].IsuListOrderByCreatedAt[0]
	// condition の read lock を取得
	isu.CondMutex.RLock()
	lastTime := isu.Conditions.Back().TimestampUnix
	isu.CondMutex.RUnlock()

	// ユーザ数を3人以上にするときはrand被る可能性あります
	prepareUserNum := 2
	var userIdx []int
	// isucon ユーザは固定で入れる
	userIdx = append(userIdx, 0)
	for i := 0; i < prepareUserNum-1; i++ {
		randomIdx := 1 + rand.Intn(len(s.normalUsers)-1)
		userIdx = append(userIdx, randomIdx)
	}

	w, err := worker.NewWorker(func(ctx context.Context, index int) {
		randomUser := s.normalUsers[userIdx[index]]
		// ユーザのAgent設定
		agt, err := s.NewAgent(agent.WithTimeout(s.prepareTimeout))
		if err != nil {
			logger.AdminLogger.Panicln(err)
		}
		_, errs := authAction(ctx, agt, randomUser.UserID)
		for _, err := range errs {
			step.AddError(err)
			return
		}
		randomUser.Agent = agt
		// check: ログイン成功
		if errs := browserGetAuthAction(ctx, randomUser.Agent, randomUser.UserID); errs != nil {
			for _, err := range errs {
				step.AddError(err)
			}
			return
		}

		// check: ユーザ情報取得
		meRes, res, err := getMeAction(ctx, randomUser.Agent)
		if err != nil {
			step.AddError(err)
			return
		}
		if meRes == nil {
			step.AddError(errorInvalid(res, "レスポンス内容が不正です。"))
			return
		}
		if meRes.JIAUserID != randomUser.UserID {
			step.AddError(errorInvalid(res, "ログインユーザと一致しません。"))
			return
		}

		// check: ISU一覧取得
		if err := BrowserAccess(ctx, randomUser.Agent, "/", HomePage); err != nil {
			step.AddError(err)
			return
		}
		isuList, res, err := getIsuAction(ctx, randomUser.Agent)
		if err != nil {
			step.AddError(err)
			return
		}

		// verify
		expected := randomUser.IsuListOrderByCreatedAt
		if errs := verifyPrepareIsuList(res, expected, isuList); errs != nil {
			for _, err := range errs {
				step.AddError(err)
			}
			return
		}

		// isuが多い場合は5個までに
		isuConter := 5
		for jiaIsuUUID, isu := range randomUser.IsuListByID {
			isuConter--
			if isuConter < 0 {
				break
			}
			// check: ISU詳細取得
			{
				if err := BrowserAccess(ctx, randomUser.Agent, "/isu/"+jiaIsuUUID, IsuDetailPage); err != nil {
					step.AddError(err)
					return
				}
				resIsu, res, err := getIsuIdAction(ctx, randomUser.Agent, jiaIsuUUID)
				if err != nil {
					step.AddError(err)
					return
				}
				if resIsu.JIAIsuUUID != jiaIsuUUID ||
					resIsu.Name != isu.Name ||
					resIsu.Character != isu.Character ||
					resIsu.ID != isu.ID {
					step.AddError(errorInvalid(res, "ユーザが所持している椅子が取得できません。"))
					return
				}
			}

			// check: ISU画像取得
			{
				imgByte, res, err := getIsuIconAction(ctx, randomUser.Agent, jiaIsuUUID)
				if err != nil {
					step.AddError(err)
					return
				}
				if err := verifyStatusCode(res, http.StatusOK); err != nil {
					step.AddError(err)
					return
				}
				expected := isu.ImageHash
				actual := md5.Sum(imgByte)
				if expected != actual {
					step.AddError(errorInvalid(res, "期待するISUアイコンと一致しません"))
					return
				}

				//競技者が304を返した来た場合に、それがうまくいってるかのチェック
				imgByte, res, err = getIsuIconAction(ctx, randomUser.Agent, jiaIsuUUID)
				if err != nil {
					step.AddError(err)
					return
				}
				actual = md5.Sum(imgByte)
				if expected != actual {
					step.AddError(errorInvalid(res, "期待するISUアイコンと一致しません"))
					return
				}
			}

			// check: ISUグラフ取得
			{
				// condition の read lock を取得
				isu.CondMutex.RLock()
				lastCond := isu.Conditions.Back()
				isu.CondMutex.RUnlock()

				// prepare中に追加したISUはconditionが無いためチェックしない
				if lastCond == nil {
					continue
				}

				if err := BrowserAccess(ctx, randomUser.Agent, "/isu/"+jiaIsuUUID+"/graph", IsuGraphPage); err != nil {
					step.AddError(err)
					return
				}

				req := service.GetGraphRequest{Date: lastCond.TimestampUnix}
				graph, res, err := getIsuGraphAction(ctx, randomUser.Agent, jiaIsuUUID, req)
				if err != nil {
					step.AddError(err)
					return
				}
				if err := verifyStatusCode(res, http.StatusOK); err != nil {
					step.AddError(err)
					return
				}
				// graphの検証
				if err := verifyPrepareGraph(res, randomUser, jiaIsuUUID, &req, graph); err != nil {
					step.AddError(err)
					return
				}

				// 前日分も検証
				yesterday := time.Unix(lastCond.TimestampUnix, 0).Add(-24 * time.Hour).Unix()
				req = service.GetGraphRequest{Date: yesterday}
				graph, res, err = getIsuGraphAction(ctx, randomUser.Agent, jiaIsuUUID, req)
				if err != nil {
					step.AddError(err)
					return
				}
				if err := verifyStatusCode(res, http.StatusOK); err != nil {
					step.AddError(err)
					return
				}
				if err := verifyPrepareGraph(res, randomUser, jiaIsuUUID, &req, graph); err != nil {
					step.AddError(err)
					return
				}
			}

			// check: ISUコンディション取得
			//	- option無し
			{
				endTime := lastTime

				// condition の read lock を取得
				isu.CondMutex.RLock()
				lastCond := isu.Conditions.Back()
				if lastCond != nil {
					endTime = lastCond.TimestampUnix
				}
				isu.CondMutex.RUnlock()

				if err := BrowserAccess(ctx, randomUser.Agent, "/isu/"+jiaIsuUUID+"/condition", IsuConditionPage); err != nil {
					step.AddError(err)
					return
				}

				req := service.GetIsuConditionRequest{
					StartTime:      nil,
					EndTime:        endTime,
					ConditionLevel: "info,warning,critical",
				}
				conditionsTmp, res, err := getIsuConditionAction(ctx, randomUser.Agent, jiaIsuUUID, req)
				if err != nil {
					step.AddError(err)
					return
				}
				//検証
				err = verifyPrepareIsuConditions(res, randomUser, jiaIsuUUID, &req, conditionsTmp)
				if err != nil {
					step.AddError(err)
					return
				}
			}

			// check: ISUコンディション取得（オプションあり1）
			// - start_timeは0-11時間前でrandom
			// - end_time指定を途中の時間で行う
			{
				endTime := lastTime

				// condition の read lock を取得
				isu.CondMutex.RLock()
				infoConditions := isu.Conditions.Info
				if len(infoConditions) != 0 {
					randomCond := infoConditions[rand.Intn(len(infoConditions))]
					endTime = randomCond.TimestampUnix
				}
				isu.CondMutex.RUnlock()

				n := rand.Intn(12)
				startTime := time.Unix(endTime, 0).Add(-time.Duration(n) * time.Hour).Unix()
				req := service.GetIsuConditionRequest{
					StartTime:      &startTime,
					EndTime:        endTime,
					ConditionLevel: "info,warning,critical",
				}

				if err := BrowserAccess(ctx, randomUser.Agent, "/isu/"+jiaIsuUUID+"/condition", IsuConditionPage); err != nil {
					step.AddError(err)
					return
				}

				conditionsTmp, res, err := getIsuConditionAction(ctx, randomUser.Agent, jiaIsuUUID, req)
				if err != nil {
					step.AddError(err)
					return
				}
				//検証
				err = verifyPrepareIsuConditions(res, randomUser, jiaIsuUUID, &req, conditionsTmp)
				if err != nil {
					step.AddError(err)
					return
				}
			}

			// check: ISUコンディション取得（オプションあり2）
			// - condition random指定
			// - start_time指定でlimitまで取得できない
			{
				endTime := lastTime

				// condition の read lock を取得
				isu.CondMutex.RLock()
				lastCond := isu.Conditions.Back()
				if lastCond != nil {
					endTime = lastCond.TimestampUnix
				}
				isu.CondMutex.RUnlock()

				var levelQuery string
				switch rand.Intn(3) {
				case 0:
					levelQuery = "info"
				case 1:
					levelQuery = "warning"
				case 2:
					levelQuery = "critical"
				}

				startTime := time.Unix(endTime, 0).Add(-1 * time.Hour).Unix()
				req := service.GetIsuConditionRequest{
					StartTime:      &startTime,
					EndTime:        endTime,
					ConditionLevel: levelQuery,
				}

				if err := BrowserAccess(ctx, randomUser.Agent, "/isu/"+jiaIsuUUID+"/condition", IsuConditionPage); err != nil {
					step.AddError(err)
					return
				}

				conditionsTmp, res, err := getIsuConditionAction(ctx, randomUser.Agent, jiaIsuUUID, req)
				if err != nil {
					step.AddError(err)
					return
				}
				//検証
				err = verifyPrepareIsuConditions(res, randomUser, jiaIsuUUID, &req, conditionsTmp)
				if err != nil {
					step.AddError(err)
					return
				}
			}
		}
		// check: ログアウト成功
		_, err = signoutAction(ctx, agt)
		if err != nil {
			step.AddError(err)
			return
		}
		// サインアウト状態であることを確認
		resBody, res, err := signoutErrorAction(ctx, agt)
		if err != nil {
			step.AddError(err)
			return
		}
		if err := verifyNotSignedIn(res, resBody); err != nil {
			step.AddError(err)
			return
		}

	}, worker.WithLoopCount(int32(prepareUserNum)))

	if err != nil {
		logger.AdminLogger.Panic(err)
	}
	w.AddParallelism(2)
	w.Process(ctx)
	w.Wait()

}

func (s *Scenario) prepareCheckAuth(ctx context.Context, step *isucandar.BenchmarkStep) error {

	//TODO: ユーザープール
	//とりあえずは使い捨てのユーザーを使う
	w, err := worker.NewWorker(func(ctx context.Context, index int) {

		agt, err := s.NewAgent(agent.WithTimeout(s.prepareTimeout))
		if err != nil {
			logger.AdminLogger.Panic(err)
			return
		}
		userID := random.UserName()
		//各種ログイン失敗ケース
		errs := authActionError(ctx, agt, userID, index%authActionErrorNum)
		for _, err := range errs {
			step.AddError(err)
		}

	}, worker.WithLoopCount(authActionErrorNum))

	if err != nil {
		logger.AdminLogger.Panic(err)
	}

	w.Process(ctx)
	//w.Wait()
	//MEMO: ctx.Done()の場合は、プロセスが終了していない可能性がある。

	//作成済みユーザーへのログイン確認
	agt, err := s.NewAgent(agent.WithTimeout(s.prepareTimeout))
	if err != nil {
		logger.AdminLogger.Panic(err)
		return nil
	}
	userID := random.UserName()

	_, errs := authAction(ctx, agt, userID)
	for _, err := range errs {
		step.AddError(err)
	}
	agt.ClearCookie()
	//二回目のログイン
	_, errs = authAction(ctx, agt, userID)
	for _, err := range errs {
		step.AddError(err)
	}

	return nil
}

func (s *Scenario) prepareIrregularCheckPostSignout(ctx context.Context, step *isucandar.BenchmarkStep) {
	// サインインしてない状態でサインアウト実行
	agt, err := s.NewAgent(agent.WithTimeout(s.prepareTimeout))
	if err != nil {
		step.AddError(err)
		return
	}
	resBody, res, err := signoutErrorAction(ctx, agt)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyNotSignedIn(res, resBody); err != nil {
		step.AddError(err)
		return
	}
}

func (s *Scenario) prepareIrregularCheckGetMe(ctx context.Context, guestAgent *agent.Agent, step *isucandar.BenchmarkStep) {
	// サインインしてない状態で取得
	resBody, res, err := getMeErrorAction(ctx, guestAgent)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyNotSignedIn(res, resBody); err != nil {
		step.AddError(err)
		return
	}
}

func (s *Scenario) prepareIrregularCheckGetIsuList(ctx context.Context, noIsuUser *model.User, guestAgent *agent.Agent, step *isucandar.BenchmarkStep) {
	// check: 椅子未所持の場合は椅子が存在しない
	if err := BrowserAccess(ctx, noIsuUser.Agent, "/", HomePage); err != nil {
		step.AddError(err)
		return
	}
	isuList, res, err := getIsuAction(ctx, noIsuUser.Agent)
	if err != nil {
		step.AddError(err)
		return
	}
	expected := noIsuUser.IsuListOrderByCreatedAt
	if errs := verifyPrepareIsuList(res, expected, isuList); errs != nil {
		for _, err := range errs {
			step.AddError(err)
		}
		return
	}

	// check: サインインしてない状態で取得
	resBody, res, err := getIsuErrorAction(ctx, guestAgent)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyNotSignedIn(res, resBody); err != nil {
		step.AddError(err)
		return
	}
}

func (s *Scenario) prepareCheckPostIsu(ctx context.Context, loginUser *model.User, noIsuUser *model.User, guestAgent *agent.Agent, step *isucandar.BenchmarkStep) {
	//Isuの登録 e.POST("/api/isu", postIsu)
	// check: 椅子の登録が成功する（デフォルト画像）
	if err := BrowserAccess(ctx, loginUser.Agent, "/register", RegisterPage); err != nil {
		step.AddError(err)
		return
	}

	isu := s.NewIsu(ctx, step, loginUser, true, nil, false)
	if isu == nil {
		return
	}

	expected := isu.ToService()
	actual, res, err := getIsuIdAction(ctx, loginUser.Agent, isu.JIAIsuUUID)
	if err != nil {
		step.AddError(err)
		return
	}
	if !reflect.DeepEqual(*actual, *expected) {
		step.AddError(errorInvalid(res, "ユーザが所持している椅子が取得できません。"))
		return
	}

	imgByte, res, err := getIsuIconAction(ctx, loginUser.Agent, isu.JIAIsuUUID)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusOK); err != nil {
		step.AddError(err)
		return
	}
	data, err := ioutil.ReadFile("./images/default.jpg")
	if err != nil {
		logger.AdminLogger.Panicln(err)
	}
	expectedImg := md5.Sum(data)
	actualImg := md5.Sum(imgByte)
	if expectedImg != actualImg {
		step.AddError(errorInvalid(res, "期待するISUアイコンと一致しません"))
		return
	}

	// check: 椅子の登録が成功する（画像あり）
	if err := BrowserAccess(ctx, loginUser.Agent, "/register", RegisterPage); err != nil {
		step.AddError(err)
		return
	}

	img, err := random.Image()
	if err != nil {
		logger.AdminLogger.Panic(err)
	}
	isuWithImg := s.NewIsu(ctx, step, loginUser, true, img, false)
	if isuWithImg == nil {
		return
	}

	expected = isuWithImg.ToService()
	actual, res, err = getIsuIdAction(ctx, loginUser.Agent, isuWithImg.JIAIsuUUID)
	if err != nil {
		step.AddError(err)
		return
	}
	if !reflect.DeepEqual(*actual, *expected) {
		step.AddError(errorInvalid(res, "ユーザが所持している椅子が取得できません。"))
		return
	}

	imgByte, res, err = getIsuIconAction(ctx, loginUser.Agent, isuWithImg.JIAIsuUUID)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusOK); err != nil {
		step.AddError(err)
		return
	}
	expectedImg = md5.Sum(img)
	actualImg = md5.Sum(imgByte)
	if expectedImg != actualImg {
		step.AddError(errorInvalid(res, "期待するISUアイコンと一致しません"))
		return
	}

	// check: サインインしてない状態で椅子登録
	req := service.PostIsuRequest{
		JIAIsuUUID: isu.JIAIsuUUID,
		IsuName:    isu.Name,
	}
	resBody, res, err := postIsuErrorAction(ctx, guestAgent, req)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyNotSignedIn(res, resBody); err != nil {
		step.AddError(err)
		return
	}

	// check: 登録済みのisuをactivate
	resBody, res, err = postIsuErrorAction(ctx, loginUser.Agent, req)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusConflict); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "duplicated: isu"); err != nil {
		step.AddError(err)
		return
	}

	// check: 他ユーザがactivate済みのisuをactivate
	resBody, res, err = postIsuErrorAction(ctx, noIsuUser.Agent, req)
	if err != nil {
		step.AddError(err)
		return
	}
	// もともとjia serviceでforbiddenで返されてたけど、バックエンド実装変更でconflictに
	if err := verifyStatusCode(res, http.StatusConflict); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "duplicated: isu"); err != nil {
		step.AddError(err)
		return
	}

	// check: 存在しない椅子を登録
	req = service.PostIsuRequest{
		JIAIsuUUID: "jiaisuuuid",
		IsuName:    "isuname",
	}
	resBody, res, err = postIsuErrorAction(ctx, loginUser.Agent, req)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "JIAService returned error"); err != nil {
		step.AddError(err)
		return
	}
}

func (s *Scenario) prepareIrregularCheckGetIsu(ctx context.Context, loginUser *model.User, noIsuUser *model.User, guestAgent *agent.Agent, step *isucandar.BenchmarkStep) {
	isu := loginUser.IsuListOrderByCreatedAt[0]
	// check: 未ログイン状態
	resBody, res, err := getIsuIdErrorAction(ctx, guestAgent, isu.JIAIsuUUID)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyNotSignedIn(res, resBody); err != nil {
		step.AddError(err)
		return
	}

	// check: 他ユーザの椅子に対するリクエスト
	resBody, res, err = getIsuIdErrorAction(ctx, noIsuUser.Agent, isu.JIAIsuUUID)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "not found: isu"); err != nil {
		step.AddError(err)
		return
	}

	// check: 存在しない椅子を取得
	resBody, res, err = getIsuIdErrorAction(ctx, loginUser.Agent, "jiaisuuuid")
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "not found: isu"); err != nil {
		step.AddError(err)
		return
	}
}

func (s *Scenario) prepareIrregularCheckGetIsuIcon(ctx context.Context, loginUser *model.User, noIsuUser *model.User, guestAgent *agent.Agent, step *isucandar.BenchmarkStep) {
	isu := loginUser.IsuListOrderByCreatedAt[0]
	// check: 未ログイン状態
	resBody, res, err := getIsuIconErrorAction(ctx, guestAgent, isu.JIAIsuUUID)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyNotSignedIn(res, resBody); err != nil {
		step.AddError(err)
		return
	}

	// check: 他ユーザの椅子画像取得
	//  - nginxキャッシュで他ユーザが見れたらダメ(cache OKにするならcache時間の検討必要そう
	resBody, res, err = getIsuIconErrorAction(ctx, noIsuUser.Agent, isu.JIAIsuUUID)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "not found: isu"); err != nil {
		step.AddError(err)
		return
	}

	// check: 登録されていない椅子に対するリクエスト
	resBody, res, err = getIsuIconErrorAction(ctx, loginUser.Agent, "jiaisuuuid")
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "not found: isu"); err != nil {
		step.AddError(err)
		return
	}

}

func (s *Scenario) prepareIrregularCheckGetIsuGraph(ctx context.Context, loginUser *model.User, noIsuUser *model.User, guestAgent *agent.Agent, step *isucandar.BenchmarkStep) {
	// check: 未ログイン状態
	isu := loginUser.IsuListOrderByCreatedAt[0]
	query := url.Values{}
	query.Set("datetime", strconv.FormatInt(time.Now().Unix(), 10))
	resBody, res, err := getIsuGraphErrorAction(ctx, guestAgent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyNotSignedIn(res, resBody); err != nil {
		step.AddError(err)
		return
	}

	// check: dateパラメータ不足
	query = url.Values{}
	resBody, res, err = getIsuGraphErrorAction(ctx, loginUser.Agent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusBadRequest); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "missing: datetime"); err != nil {
		step.AddError(err)
		return
	}

	// check: dateパラメータのフォーマット違反
	query = url.Values{}
	query.Set("datetime", "datetime")
	resBody, res, err = getIsuGraphErrorAction(ctx, loginUser.Agent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusBadRequest); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "bad format: datetime"); err != nil {
		step.AddError(err)
		return
	}

	// check: 他ユーザの椅子に対するリクエスト
	query = url.Values{}
	query.Set("datetime", strconv.FormatInt(time.Now().Unix(), 10))
	resBody, res, err = getIsuGraphErrorAction(ctx, noIsuUser.Agent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "not found: isu"); err != nil {
		step.AddError(err)
		return
	}

	// check: 登録されていない椅子に対するリクエスト
	query = url.Values{}
	query.Set("datetime", strconv.FormatInt(time.Now().Unix(), 10))
	resBody, res, err = getIsuGraphErrorAction(ctx, loginUser.Agent, "jiaisuuuid", query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "not found: isu"); err != nil {
		step.AddError(err)
		return
	}
}

func (s *Scenario) prepareIrregularCheckGetIsuConditions(ctx context.Context, loginUser *model.User, noIsuUser *model.User, guestAgent *agent.Agent, step *isucandar.BenchmarkStep) {
	isu := loginUser.IsuListOrderByCreatedAt[0]

	// condition の read lock を取得
	isu.CondMutex.RLock()
	lastTime := isu.Conditions.Back().TimestampUnix
	isu.CondMutex.RUnlock()

	// check: 未ログイン状態
	query := url.Values{}
	query.Set("end_time", strconv.FormatInt(lastTime, 10))
	query.Set("condition_level", "info,warning,critical")

	isu = loginUser.IsuListOrderByCreatedAt[0]
	resBody, res, err := getIsuConditionErrorAction(ctx, guestAgent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyNotSignedIn(res, resBody); err != nil {
		step.AddError(err)
		return
	}

	// check: end_timeパラメータ不足
	query = url.Values{}
	query.Set("condition_level", "info,warning,critical")

	resBody, res, err = getIsuConditionErrorAction(ctx, loginUser.Agent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusBadRequest); err != nil {
		step.AddError(err)
		return
	}
	// MEMO: 他と違ってパラメータ不足がXXX is missingではなくbad format扱い
	if err := verifyText(res, resBody, "bad format: end_time"); err != nil {
		step.AddError(err)
		return
	}

	// check: end_time不正パラメータ
	query = url.Values{}
	query.Set("end_time", "end_time")
	query.Set("condition_level", "info,warning,critical")
	resBody, res, err = getIsuConditionErrorAction(ctx, loginUser.Agent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusBadRequest); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "bad format: end_time"); err != nil {
		step.AddError(err)
		return
	}

	// check: condition_levelパラメータ不足(空文字含む)
	query = url.Values{}
	query.Set("end_time", strconv.FormatInt(lastTime, 10))
	resBody, res, err = getIsuConditionErrorAction(ctx, loginUser.Agent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusBadRequest); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "missing: condition_level"); err != nil {
		step.AddError(err)
		return
	}

	// check: start_timeフォーマット違反
	query = url.Values{}
	query.Set("end_time", strconv.FormatInt(lastTime, 10))
	query.Set("condition_level", "info,warning,critical")
	query.Set("start_time", "start_time")
	resBody, res, err = getIsuConditionErrorAction(ctx, loginUser.Agent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusBadRequest); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "bad format: start_time"); err != nil {
		step.AddError(err)
		return
	}

	// check: 他ユーザの椅子に対するリクエスト
	query = url.Values{}
	query.Set("end_time", strconv.FormatInt(lastTime, 10))
	query.Set("condition_level", "info,warning,critical")
	resBody, res, err = getIsuConditionErrorAction(ctx, noIsuUser.Agent, isu.JIAIsuUUID, query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "not found: isu"); err != nil {
		step.AddError(err)
		return
	}

	// check: 登録されていない椅子に対するリクエスト
	query = url.Values{}
	query.Set("end_time", strconv.FormatInt(lastTime, 10))
	query.Set("condition_level", "info,warning,critical")
	resBody, res, err = getIsuConditionErrorAction(ctx, loginUser.Agent, "jiaisuuuid", query)
	if err != nil {
		step.AddError(err)
		return
	}
	if err := verifyStatusCode(res, http.StatusNotFound); err != nil {
		step.AddError(err)
		return
	}
	if err := verifyText(res, resBody, "not found: isu"); err != nil {
		step.AddError(err)
		return
	}
}
