// Exercise the production accumulator without loading a model. Checks stay on in Release.
#define main whisper_stream_main
#include "main.cpp"
#undef main
#include <functional>
struct Piece { std::string bytes; int64_t start, end; };
int main() {
    int failures = 0;
    auto check = [&](const char * name, std::initializer_list<Piece> pieces, const std::string & expected,
                     bool vad = true, double locked = 0, double previous = 0) {
        try {
            double candidate = previous;
            TokenAccumulator a{J::array(), "", "", -1, -1, vad, {16000,32000}, 160000, 192000, locked, candidate};
            for (const auto & p : pieces) a.add(p.bytes, p.start, p.end);
            J items = J::array(); a.finish(items);
            auto encoded = items.dump(); // Strict serializer: no replace/ignore.
            require(J::parse(encoded) == items, "roundtrip");
            std::string actual;
            double last = std::max(locked, previous);
            for (const auto & item : items) {
                std::string words;
                for (const auto & w : item["words"]) {
                    require(w["start_sec"].get<double>() >= last && w["end_sec"].get<double>() > w["start_sec"].get<double>() && w["end_sec"].get<double>() <= 12, "timeline");
                    last = w["end_sec"]; words += w["text"].get<std::string>();
                }
                require(words == item["text"], "word_text"); actual += words;
            }
            require(actual == expected, "text");
            std::cout << "PASS " << name << '\n';
        } catch (const std::exception & e) { ++failures; std::cerr << "FAIL " << name << ": " << e.what() << '\n'; }
    };
    const std::string zh = "\xe4\xb8\xad";
    check("Chinese split", {{"\xe4\xb8",0,10},{"\xad",10,20}}, zh);
    check("incomplete tail", {{"ok",0,10},{"\xe4\xb8",10,20}}, "ok");
    check("incomplete only", {{"\xe4",0,10}}, "");
    check("zero duration continuation", {{"\xe4\xb8",0,10},{"\xad",10,10},{"A",10,20}}, zh+"A");
    check("VAD boundary continuation", {{"\xe4\xb8",90,100},{"\xad",100,110}}, zh);
    check("discarded group cannot poison next", {{"\xe4\xb8",100,110},{"\xad",110,120},{"A",20,30}}, "A");
    check("invalid cannot poison next", {{"\xff",0,10},{"A",10,20}}, "A");
    check("valid trailing whitespace", {{"ok",0,10},{" \xe3\x80\x80",10,20}}, "ok \xe3\x80\x80");
    check("whitespace only", {{" \xe3\x80\x80",0,10}}, "");
    check("invalid after whitespace", {{"ok",0,10},{" ",10,20},{"\xe4",20,30}}, "ok ");
    check("locked group", {{"\xe4\xb8",0,10},{"\xad",10,20},{"A",30,40}}, "A", true, 11.2, 11.2);
    check("monotonic", {{"A",20,40},{"B",30,50}}, "AB");
    check("non VAD tail", {{"ok",0,10},{"\xe4",10,20}}, "ok", false);
    check("non VAD Chinese", {{"\xe4\xb8",0,10},{"\xad",10,20}}, zh, false);
    auto segments = [&](const char * name, std::initializer_list<Piece> second, const std::string & expected,
                        double expected_end) {
        try {
            double candidate = 0;
            J items = J::array();
            TokenAccumulator first{J::array(), "", "", -1, -1, false, {0,32000}, 0, 32000, 0, candidate};
            first.add("A", 0, 100); first.finish(items);
            TokenAccumulator next{J::array(), "", "", -1, -1, false, {0,32000}, 0, 32000, 0, candidate};
            for (const auto & p : second) next.add(p.bytes, p.start, p.end);
            next.finish(items);
            require(J::parse(items.dump()) == items, "roundtrip");
            double last = 0; std::string actual;
            for (const auto & item : items) {
                require(item["start_sec"].get<double>() >= last, "segment_timeline");
                std::string words;
                for (const auto & w : item["words"]) {
                    require(w["start_sec"].get<double>() >= last && w["end_sec"].get<double>() > w["start_sec"].get<double>(), "word_timeline");
                    last = w["end_sec"]; words += w["text"].get<std::string>();
                }
                require(item["start_sec"] == item["words"][0]["start_sec"] && item["end_sec"] == last, "item_bounds");
                require(words == item["text"], "word_text"); actual += words;
            }
            require(actual == expected, "text");
            require(last == expected_end && candidate == last, "candidate_end");
            std::cout << "PASS " << name << '\n';
        } catch (const std::exception & e) { ++failures; std::cerr << "FAIL " << name << ": " << e.what() << '\n'; }
    };
    segments("non VAD cross segment overlap", {{"B",97,120}}, "AB", 1.2);
    segments("non VAD cross segment fully clipped", {{"drop",90,100},{"B",100,120}}, "AB", 1.2);
    segments("non VAD cross segment zero duration", {{"drop",97,97},{"B",100,120}}, "AB", 1.2);
    segments("non VAD cross segment split UTF8", {{"\xe4\xb8",97,100},{"\xad",100,120}}, "A"+zh, 1.2);
    segments("non VAD cross segment whitespace", {{" ",97,110},{"B",105,120},{" ",120,130}}, "A B ", 1.2);
    check("non VAD locked group", {{"drop",0,10},{"B",30,40}}, "B", false, 10.2, 10.2);
    try {
        auto item = [](double start, double end, const char * text) {
            return J{{"start_sec",start},{"end_sec",end},{"text",text},
                     {"words",J::array({J{{"start_sec",start},{"end_sec",end},{"text",text}}})}};
        };
        J previous = J::array({item(0,1,"A"), item(1,4.20,"B")});
        J next = J::array({item(0,.98,"A"), item(1,4.17,"B"), item(4.17,5.96,"C")});
        J decoded = next;
        preserve_candidates(next, previous, .2);
        require(next[0] == previous[0] && next[1] == decoded[1] && next[2] == decoded[2], "unsafe_reuse");
        J safe = J::array({item(0,.98,"A"), item(1,4.17,"B"), item(4.21,5.96,"C")});
        preserve_candidates(safe, previous, .2);
        require(safe[0] == previous[0] && safe[1] == previous[1], "safe_reuse");
        require(J::parse(next.dump()) == next && J::parse(safe.dump()) == safe, "roundtrip");
        std::cout << "PASS stable candidate reuse cannot reintroduce overlap\n";
    } catch (const std::exception & e) { ++failures; std::cerr << "FAIL stable candidate reuse: " << e.what() << '\n'; }
    return failures ? 1 : 0;
}
