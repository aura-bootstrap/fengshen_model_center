// Pure timeline tests; no model or inference is required.
#define main whisper_stream_main
#include "main.cpp"
#undef main
#include <cassert>
int main() {
    for (uint64_t revision : {uint64_t(0), uint64_t(7)}) {
        auto error = envelope("error", "request", 2, revision);
        error["error_code"] = "window_unstable";
        assert(error == J({{"protocol",protocol},{"type","error"},{"request_id","request"},
                           {"episode_number",2},{"revision",revision},{"error_code","window_unstable"}}));
    }
    auto a = vad_range(123, 245, 50000, 0);
    assert(a.start == 19680 && a.end == 39200); // API units are centiseconds.
    auto b = vad_range(240, 400, 50000, a.end);
    assert(b.start == 39200 && b.end == 50000); // Clip overlap and received tail.
    auto empty = vad_range(500, 600, 50000, b.end);
    assert(empty.start == empty.end);
    double start, end;
    assert(vad_token_time(a, 160000, 210000, 0, 20, 0, 0, start, end));
    assert(std::abs(start-11.23) < 1e-9 && std::abs(end-11.43) < 1e-9);
    assert(vad_token_time(a, 160000, 210000, 120, 500, 0, 0, start, end));
    assert(std::abs(end-12.45) < 1e-9); // Never decode time across removed silence.
    assert(!vad_token_time(a, 160000, 210000, 0, 20, 11.3, 0, start, end));
    assert(!vad_token_time(a, 160000, 210000, 0, 20, 0, 11.5, start, end));
    assert(!vad_token_time(a, 160000, 210000, 200, 300, 0, 0, start, end));
    assert(vad_token_time(b, 160000, 205000, 0, 500, 0, 12.45, start, end));
    assert(std::abs(start-12.45) < 1e-9 && std::abs(end-12.8125) < 1e-9);
    auto separated = vad_range(400, 480, 80000, a.end);
    assert(vad_token_time(separated, 160000, 240000, 10, 30, 0, 12.45, start, end));
    assert(std::abs(start-14.1) < 1e-9 && std::abs(end-14.3) < 1e-9);
    // The 1.55 second silence remains on the original timeline.
    assert(!vad_token_time(separated, 160000, 240000, 10, 30, 14.2, 0, start, end));
    for (auto v : {NAN, INFINITY, -1.f}) {
        bool failed = false;
        try { vad_range(v, 1, 16000, 0); } catch (const Failure &) { failed = true; }
        assert(failed);
    }
    return 0;
}
