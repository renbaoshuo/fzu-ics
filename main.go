package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
	"github.com/west2-online/jwch"
)

// 调课规则 API
const ADJUST_API = "https://fzuhelper.west2.online/api/v1/course/adjust/list"

// AdjustRule 调课规则
type AdjustRule struct {
	ID          int    `json:"id"`
	Enabled     bool   `json:"enabled"`     // 是否启用调课规则
	Year        string `json:"year"`        // 年份
	Term        string `json:"term"`        // 学期
	FromDate    string `json:"from_date"`   // 原上课日期 YYYY-MM-DD
	FromWeek    int    `json:"from_week"`   // 原上课周数
	FromWeekday int    `json:"from_weekday"` // 原上课星期 1-7
	ToDate      string `json:"to_date"`     // 新上课日期 YYYY-MM-DD，为空表示仅放假停课
	ToWeek      int    `json:"to_week"`     // 新上课周数
	ToWeekday   int    `json:"to_weekday"`  // 新上课星期 1-7
}

// 作息时间
var CLASS_TIME = [][2][2]int{
	{{0, 0}, {23, 59}}, // [[起始小时, 起始分钟], [结束小时, 结束分钟]]
	{{8, 20}, {9, 5}},  // 1
	{{9, 15}, {10, 0}},
	{{10, 20}, {11, 5}},
	{{11, 15}, {12, 0}},
	{{14, 0}, {14, 45}},
	{{14, 55}, {15, 40}},
	{{15, 50}, {16, 35}},
	{{16, 45}, {17, 30}},
	{{19, 0}, {19, 45}},
	{{19, 55}, {20, 40}},
	{{20, 50}, {21, 35}}, // 11
}

var GEO = map[string][2]float64{
	"铜盘A":        {26.10377684575211, 119.26204839259863},
	"铜盘B":        {26.10316987108786, 119.26238098686404},
	"铜盘教学楼":      {26.103533518379862, 119.26256559252518},
	"铜盘田径场":      {26.104075916665806, 119.26411644200412},
	"铜盘科报厅A":     {26.103566850977305, 119.26282667315306},
	"铜盘科报厅B":     {26.103443904554403, 119.26236726136176},
	"旗山东2":       {26.060826364749953, 119.196402584604},
	"旗山东3":       {26.061063932388176, 119.1974455510677},
	"旗山中":        {26.059990869949978, 119.19556464641397},
	"旗山西1":       {26.05936405825869, 119.19537886898759},
	"旗山西2":       {26.05893356673894, 119.19539621561447},
	"旗山西3":       {26.058541512922556, 119.19543335852326},
	"旗山文1":       {26.062080021896797, 119.19891554961018},
	"旗山文2":       {26.062099408666285, 119.19923063579931},
	"旗山文3":       {26.06231460261737, 119.19908819973453},
	"旗山文4":       {26.063153486917383, 119.19892242008977},
	"旗山公语":       {26.059990869949978, 119.19556464641397},
	"旗山物理实验教学中心": {26.064036932218578, 119.20031495781095},
	"旗山游泳池":      {26.052256463877352, 119.1977143081377},
	"旗山计数3":      {26.06220633314379, 119.20122043219848},
}

func main() {
	// 初始化
	var cstSh, _ = time.LoadLocation("Asia/Shanghai")
	time.Local = cstSh

	// 读入信息（优先读取环境变量 FZU_ID / FZU_PASSWORD）
	id := readInput("FZU_ID", "请输入学号: ")
	password := readInput("FZU_PASSWORD", "请输入密码: ")

	// 创建学生对象
	stu := jwch.NewStudent().WithUser(id, password)

	// 登录
	err := stu.Login()
	solveErr(err)

	fmt.Println("登录成功！")

	// 获取学期列表
	terms, err := stu.GetTerms()
	solveErr(err)

	fmt.Println("========")
	fmt.Println("学期列表:", strings.Join(terms.Terms, " "))

	needTerm := readInput("FZU_TERM", "请输入学期 (all): ")

	if needTerm == "" || needTerm == "all" {
		needTerm = "all"
	} else if !contains(terms.Terms, needTerm) {
		fmt.Println("无效学期！")
		return
	}

	// 获取校历
	calendar, err := stu.GetSchoolCalendar()
	solveErr(err)

	// 转换为 ics 格式
	cal := ics.NewCalendar()
	cal.SetMethod(ics.MethodRequest)
	cal.SetXWRCalName(fmt.Sprintf("福州大学课程表 [%s]", id))
	cal.SetTimezoneId("Asia/Shanghai")
	cal.SetXWRTimezone("Asia/Shanghai")

	if needTerm == "all" {
		for _, term := range terms.Terms {
			addTermToCalendar(stu, cal, calendar, term, terms.ViewState, terms.EventValidation)
		}
	} else {
		addTermToCalendar(stu, cal, calendar, needTerm, terms.ViewState, terms.EventValidation)
	}

	// 写入文件
	fmt.Println("========")
	filename := os.Getenv("FZU_OUTPUT")
	if filename == "" {
		filename = fmt.Sprintf("福州大学课程表 [%s] (%s).ics", id, needTerm)
	}
	fmt.Println("写入文件", filename)

	calendarContent := cal.Serialize()
	err = os.WriteFile(filename, []byte(calendarContent), 0644)
	solveErr(err)

	fmt.Println("写入成功！")
	fmt.Println("========")
}

func addTermToCalendar(stu *jwch.Student, cal *ics.Calendar, schoolCal *jwch.SchoolCalendar, term string, viewState string, eventValidation string) {
	var curTermStartDate time.Time
	var err error

	// 获取调课规则
	exDates, err := fetchAdjustRules(term)
	if err != nil {
		fmt.Printf("获取 [%s] 调课规则失败: %v\n", term, err)
		exDates = make(map[string]*string)
	} else {
		fmt.Printf("[%s] 找到 %d 条调课规则\n", term, len(exDates))
	}

	// 查找学期开始时间
	for _, item := range schoolCal.Terms {
		if item.Term == term {
			curTermStartDate, err = time.Parse("2006-01-02", item.StartDate)
			solveErr(err)
		}
	}

	if curTermStartDate.IsZero() {
		fmt.Printf("未找到学期 [%s] 开始时间！\n", term)
		return
	}

	// 使用学期开始时间的周一作为第 1 周的开始
	// 好像教务处的校历是从周一开始的，所以不用动
	dateBase := curTermStartDate

	// 获取课程表
	list, err := stu.GetSemesterCourses(term, viewState, eventValidation)
	solveErr(err)

	fmt.Printf("[%s] 找到 %d 门课程\n", term, len(list))

	addCoursesToCalendar(cal, term, list, dateBase, exDates)
}

func addCoursesToCalendar(cal *ics.Calendar, term string, courses []*jwch.Course, dateBase time.Time, exDates map[string]*string) {
	for _, course := range courses {
		if strings.HasSuffix(course.ExamType, "补考") {
			continue
		}

		name := course.Name
		teacher := course.Teacher
		description := "任课教师：" + teacher + "\n"

		// 普通课程
		for _, scheduleRule := range course.ScheduleRules {
			if scheduleRule.FromFullWeek {
				continue // 在下面单独处理整周课程
			}

			displayName := name
			displayDescription := description
			lat, lon := findGeoLocation(scheduleRule.Location)
			location := strings.TrimPrefix(scheduleRule.Location, "旗山")
			startClass := scheduleRule.StartClass
			endClass := scheduleRule.EndClass
			startWeek := scheduleRule.StartWeek
			endWeek := scheduleRule.EndWeek
			weekday := scheduleRule.Weekday
			single := scheduleRule.Single
			double := scheduleRule.Double
			adjust := scheduleRule.Adjust

			/*
				利用单双周信息更新起始周数
				01-16 星期5:7-8节(双) 旗山东2-402
			*/
			if single && !double {
				startWeek = startWeek + (startWeek-1)%2
			}
			if double && !single {
				startWeek = startWeek + startWeek%2
			}

			startTime, endTime := calcClassTime(startWeek, weekday, startClass, endClass, dateBase)
			_, repeatEndTime := calcClassTime(endWeek, weekday, startClass, endClass, dateBase)
			eventIdBase := fmt.Sprintf("%s__%s_%s_%d-%d_%d_%d-%d_%s_%t_%t", term, name, teacher, startWeek, endWeek, weekday, startClass, endClass, location, single, double)

			if adjust {
				displayName = "[调课] " + displayName
				displayDescription += "本课程为教师手动调课后的课程。\n"
			}

			event := cal.AddEvent(md5Str(eventIdBase))
			event.SetCreatedTime(dateBase)
			event.SetDtStampTime(time.Now())
			event.SetModifiedAt(time.Now())
			event.SetSummary(displayName)
			event.SetDescription(displayDescription)
			event.SetLocation(location)
			if lat != 0 && lon != 0 {
				event.SetGeo(lat, lon)
				addAppleStructuredLocation(event, location, lat, lon)
			}
			event.SetStartAt(startTime)
			event.SetEndAt(endTime)

			// 添加重复规则
			if single && double { // 单双周都有
				// RRULE:FREQ=WEEKLY;UNTIL=20170101T000000Z
				event.AddRrule("FREQ=WEEKLY;UNTIL=" + repeatEndTime.UTC().Format("20060102T150405Z"))
			} else {
				// RRULE:FREQ=WEEKLY;UNTIL=20170101T000000Z;INTERVAL=2
				event.AddRrule("FREQ=WEEKLY;UNTIL=" + repeatEndTime.UTC().Format("20060102T150405Z") + ";INTERVAL=2")
			}

			// 处理调课信息，添加EXDATE
			addExDateAndRescheduledEvents(cal, event, exDates, startWeek, endWeek, weekday, startClass, endClass, dateBase, single, double, name, teacher, location, displayDescription, lat, lon)
		}

		// 整周课程
		for _, fullWeekScheduleRule := range course.FullWeekScheduleRules {
			startTime, _ := calcClassTime(fullWeekScheduleRule.StartWeek, fullWeekScheduleRule.StartWeekDay, 0, 0, dateBase)
			_, repeatEndTime := calcClassTime(fullWeekScheduleRule.EndWeek, fullWeekScheduleRule.EndWeekDay, 0, 0, dateBase)

			eventIdBase := fmt.Sprintf("%s__%s_%s_%d-%d_%d-%d", term, name, teacher, fullWeekScheduleRule.StartWeek, fullWeekScheduleRule.EndWeek, fullWeekScheduleRule.StartWeekDay, fullWeekScheduleRule.EndWeekDay)

			event := cal.AddEvent(md5Str(eventIdBase))
			event.SetCreatedTime(dateBase)
			event.SetDtStampTime(time.Now())
			event.SetModifiedAt(time.Now())
			event.SetSummary(name)
			event.SetDescription(description)
			event.SetAllDayStartAt(startTime)
			event.SetAllDayEndAt(repeatEndTime.AddDate(0, 0, 1))
		}
	}
}

func calcClassTime(week int, weekday int, startClass int, endClass int, dateBase time.Time) (time.Time, time.Time) {
	startHour, startMinute := CLASS_TIME[startClass][0][0], CLASS_TIME[startClass][0][1]
	endHour, endMinute := CLASS_TIME[endClass][1][0], CLASS_TIME[endClass][1][1]

	startTime := dateBase.AddDate(0, 0, (week-1)*7+(weekday-1))
	startTime = time.Date(startTime.Year(), startTime.Month(), startTime.Day(), startHour, startMinute, 0, 0, time.Local)
	endTime := dateBase.AddDate(0, 0, (week-1)*7+(weekday-1))
	endTime = time.Date(endTime.Year(), endTime.Month(), endTime.Day(), endHour, endMinute, 0, 0, time.Local)

	return startTime, endTime
}

func findGeoLocation(location string) (float64, float64) {
	for key, value := range GEO {
		if strings.HasPrefix(location, key) {
			return value[0], value[1]
		}
	}

	return 0, 0
}

// addAppleStructuredLocation 添加 Apple 日历使用的结构化地点字段，
// 避免 iOS 其根据“西1-201”之类的教室名匹配到无关的地图地点。
func addAppleStructuredLocation(event *ics.VEvent, location string, lat, lon float64) {
	event.AddProperty(
		ics.ComponentProperty("X-APPLE-STRUCTURED-LOCATION"),
		fmt.Sprintf("geo:%.15f,%.15f", lat, lon),
		ics.WithValue(string(ics.ValueDataTypeUri)),
		&ics.KeyValues{Key: "X-TITLE", Value: []string{location}},
	)
}

// fetchAdjustRules 从 fzuhelper API 获取指定学期的调课规则，
// 返回 原上课日期 -> 新上课日期 的映射（值为 nil 表示仅放假停课）
func fetchAdjustRules(term string) (map[string]*string, error) {
	resp, err := http.Get(ADJUST_API + "?term=" + url.QueryEscape(term))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Code    string       `json:"code"`
		Message string       `json:"message"`
		Data    []AdjustRule `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.Code != "10000" {
		return nil, fmt.Errorf("error code: %s, error msg: %s", result.Code, result.Message)
	}

	exDates := make(map[string]*string)
	for _, rule := range result.Data {
		if !rule.Enabled {
			continue
		}
		if rule.ToDate != "" {
			toDate := rule.ToDate
			exDates[rule.FromDate] = &toDate
		} else {
			exDates[rule.FromDate] = nil
		}
	}

	return exDates, nil
}

// addExDateAndRescheduledEvents 处理调课信息，添加EXDATE和调课事件
func addExDateAndRescheduledEvents(cal *ics.Calendar, event *ics.VEvent, exDates map[string]*string, startWeek, endWeek, weekday, startClass, endClass int, dateBase time.Time, single, double bool, name, teacher, location, description string, lat, lon float64) {
	// 遍历所有调课日期
	for exDateStr, rescheduleDateStr := range exDates {
		exDate, err := time.Parse("2006-01-02", exDateStr)
		if err != nil {
			continue
		}

		// 检查这个日期是否在课程时间范围内
		if isDateInCourseSchedule(exDate, startWeek, endWeek, weekday, dateBase, single, double) {
			// 计算原本应该上课的具体时间（使用本地时区）
			originalStartTime := time.Date(exDate.Year(), exDate.Month(), exDate.Day(),
				CLASS_TIME[startClass][0][0], CLASS_TIME[startClass][0][1], 0, 0, time.Local)

			// 转换为 UTC 时间，然后格式化为 EXDATE
			originalStartTimeUTC := originalStartTime.UTC()
			event.AddExdate(originalStartTimeUTC.Format("20060102T150405Z"))

			// 如果有调课目标日期，创建新事件
			if rescheduleDateStr != nil {
				rescheduleDate, err := time.Parse("2006-01-02", *rescheduleDateStr)
				if err != nil {
					continue
				}

				// 计算调课事件的开始和结束时间
				rescheduleStartTime := time.Date(rescheduleDate.Year(), rescheduleDate.Month(), rescheduleDate.Day(),
					CLASS_TIME[startClass][0][0], CLASS_TIME[startClass][0][1], 0, 0, time.Local)
				rescheduleEndTime := time.Date(rescheduleDate.Year(), rescheduleDate.Month(), rescheduleDate.Day(),
					CLASS_TIME[endClass][1][0], CLASS_TIME[endClass][1][1], 0, 0, time.Local)

				// 创建调课事件
				rescheduleEventId := fmt.Sprintf("reschedule__%s_%s__%s_%s__%s", exDateStr, *rescheduleDateStr, name, location, rescheduleStartTime.UTC().Format("20060102T150405Z"))
				rescheduleEvent := cal.AddEvent(md5Str(rescheduleEventId))
				rescheduleEvent.SetCreatedTime(dateBase)
				rescheduleEvent.SetDtStampTime(time.Now())
				rescheduleEvent.SetModifiedAt(time.Now())
				rescheduleEvent.SetSummary("[调课] " + name)
				rescheduleEvent.SetDescription(description + fmt.Sprintf("调课信息：原定于 %s 的课程因假期调休调整至 %s。\n", exDateStr, *rescheduleDateStr))
				rescheduleEvent.SetLocation(location)
				if lat != 0 && lon != 0 {
					rescheduleEvent.SetGeo(lat, lon)
					addAppleStructuredLocation(rescheduleEvent, location, lat, lon)
				}
				rescheduleEvent.SetStartAt(rescheduleStartTime)
				rescheduleEvent.SetEndAt(rescheduleEndTime)
			}
		}
	}
}

// isDateInCourseSchedule 检查指定日期是否在课程安排范围内
func isDateInCourseSchedule(date time.Time, startWeek, endWeek, weekday int, dateBase time.Time, single, double bool) bool {
	// 计算日期对应的周数和星期
	daysDiff := int(date.Sub(dateBase).Hours() / 24)
	weekNum := daysDiff/7 + 1
	dayOfWeek := int(date.Weekday())
	if dayOfWeek == 0 { // Sunday
		dayOfWeek = 7
	}

	// 检查是否在周数范围内
	if weekNum < startWeek || weekNum > endWeek {
		return false
	}

	// 检查是否是正确的星期
	if dayOfWeek != weekday {
		return false
	}

	// 检查单双周
	if single && !double && weekNum%2 == 0 {
		return false
	}
	if double && !single && weekNum%2 == 1 {
		return false
	}

	return true
}

func md5Str(str string) string {
	hasher := md5.New()
	hasher.Write([]byte(str))
	fullHash := hex.EncodeToString(hasher.Sum(nil)) // 32-bit (full) hash

	return fullHash
}

// readInput 优先读取指定环境变量，未设置时交互式读入
func readInput(envKey string, prompt string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}

	fmt.Print(prompt)
	var s string
	fmt.Scan(&s)

	return s
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}

	return false
}

func solveErr(err error) {
	if err != nil {
		panic(err)
	}
}
