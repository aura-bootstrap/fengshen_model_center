#define main batch_entry_not_called
#include "main.cpp"
#undef main
#include <cassert>
#include <functional>
static void rejects(std::function<void()> f) { bool rejected=false; try {f();} catch(...) {rejected=true;} assert(rejected); }
static std::string wav(uint32_t samples=160) {
    std::string s;
    auto put=[&](uint32_t n,int count) { for(int i=0;i<count;++i) s.push_back(static_cast<char>(n>>(8*i))); };
    s+="RIFF"; put(36+samples*2,4); s+="WAVEfmt "; put(16,4); put(1,2); put(1,2); put(16000,4); put(32000,4); put(2,2); put(16,2); s+="data"; put(samples*2,4); s.resize(44+samples*2); return s;
}
int main() {
    const char * args[]={"whisper-batch","-m","model","-f","audio","-l","zh","-t","4","-ml","1","-sow","-ojf","-of","output","-np","-ng"};
    auto parsed=batch_args(17,const_cast<char **>(args)); assert(parsed.model=="model" && parsed.wav=="audio" && parsed.output=="output");
    rejects([&]{batch_args(16,const_cast<char **>(args));});
    auto old=args[6]; args[6]="auto"; rejects([&]{batch_args(17,const_cast<char **>(args));}); args[6]=old;
    args[16]="--vad"; rejects([&]{batch_args(17,const_cast<char **>(args));}); args[16]="-ng";
    const char * vad_args[]={"whisper-batch","-m","model","-f","audio","-l","zh","-t","4","-ml","1","-sow","-ojf","-of","output","-np","-ng","--vad","--vad-model","trusted-vad"};
    // Run acceptance first so the old 17-only implementation gives a runtime RED.
    batch_args(20,const_cast<char **>(vad_args));
    for(int count:{0,1,16,18,19,21}) rejects([&]{batch_args(count,const_cast<char **>(vad_args));});
    for(auto index:{1,3,5,6,7,8,9,10,11,12,13,15,16,17,18}) {
        auto saved=vad_args[index];
        for(auto bad:{"", "--unknown", "--vad", "--vad-model"}) {
            if(std::string(saved)==bad) continue;
            vad_args[index]=bad; rejects([&]{batch_args(20,const_cast<char **>(vad_args));});
        }
        vad_args[index]=saved;
    }
    for(auto index:{2,4,14,19}) { auto saved=vad_args[index]; vad_args[index]=""; rejects([&]{batch_args(20,const_cast<char **>(vad_args));}); vad_args[index]=saved; }
    for(auto bad:{"--vad","--vad-model","--unknown"}) { vad_args[19]=bad; rejects([&]{batch_args(20,const_cast<char **>(vad_args));}); } vad_args[19]="trusted-vad";
    auto bytes=wav(); std::istringstream input(bytes); assert(read_wav(input,bytes.size()).size()==160);
    for(auto index:{0,8,20,22,24,28,32,34,40}) { auto bad=bytes; bad[index]^=1; rejects([&]{std::istringstream in(bad);read_wav(in,bad.size());}); }
    rejects([&]{std::istringstream in(bytes);read_wav(in,wav_limit+1);});
    rejects([&]{auto bad=bytes.substr(0,bytes.size()-1); std::istringstream in(bad);read_wav(in,bytes.size());});
    rejects([&]{auto bad=bytes; bad[40]=0;bad[41]=0;std::istringstream in(bad);read_wav(in,bad.size());});
    rejects([&]{auto bad=bytes; uint32_t n=7200u*rate*2+2;for(int i=0;i<4;++i)bad[40+i]=static_cast<char>(n>>(8*i));std::istringstream in(bad);read_wav(in,44ull+n);});
    double end=0; J next=J::array();
    TokenAccumulator tokens{J::array(),"","",-1,-1,false,{0,160000},0,160000,0,end};
    tokens.add("\xe4",0,10); tokens.add("\xbd\xa0",10,10); tokens.add(" hi",5,20); tokens.finish(next);
    assert(next.size()==1 && next[0]["text"]=="\xe4\xbd\xa0 hi" && next[0]["words"].size()==2);
    assert(next[0]["words"][1]["start_sec"]==.1);
    J doc={{"transcription",J::array()}};size_t budget=32;batch_segment(doc,next[0],budget);
    assert(doc["transcription"][0]["text"]=="\xe4\xbd\xa0 hi");
    assert(doc["transcription"][0]["tokens"][1]["offsets"]["from"]==100);
    assert(J::parse(batch_json(doc))==doc);
    TokenAccumulator later{J::array(),"","",-1,-1,false,{0,160000},0,160000,0,end}; J more=J::array();later.add("later",1,30);later.finish(more);assert(more[0]["start_sec"]==.2);
    rejects([&]{budget=batch_limit;batch_segment(doc,next[0],budget);});
    auto huge=doc;huge["transcription"][0]["text"]=std::string(batch_limit,'x');rejects([&]{batch_json(huge);});
    rejects([&]{batch_json(J{{"transcription",J::array()}});});
    auto vad_parsed=batch_args(20,const_cast<char **>(vad_args));
    assert(vad_parsed.vad && vad_parsed.vad_model=="trusted-vad" && !parsed.vad && parsed.vad_model.empty());
    J vad_doc={{"transcription",J::array()}}; size_t vad_budget=32; double vad_end=0;
    auto first=vad_range(123,180,5*rate,0); assert(first.start==19680 && first.end==28800);
    TokenAccumulator first_tokens{J::array(),"","",-1,-1,true,first,0,5*rate,0,vad_end};
    batch_token(first_tokens,"<|nospeech|>",0,1); batch_token(first_tokens,"[_BEG_]",0,1);
    batch_token(first_tokens,"\xe4",0,10); // First upstream segment ends mid-character.
    batch_token(first_tokens,"\xbd\xa0",10,10); batch_token(first_tokens," clipped",5,100);
    batch_finish(vad_doc,first_tokens,vad_budget);
    assert(vad_end==1.8 && vad_doc["transcription"].size()==2);
    assert(vad_doc["transcription"][0]["text"]=="\xe4\xbd\xa0");
    assert(vad_doc["transcription"][0]["offsets"]["from"]==1230);
    assert(vad_doc["transcription"][1]["offsets"]["from"]==1330 && vad_doc["transcription"][1]["offsets"]["to"]==1800);
    auto second=vad_range(400,600,5*rate,first.end); assert(second.start==64000 && second.end==80000);
    TokenAccumulator second_tokens{J::array(),"","",-1,-1,true,second,0,5*rate,0,vad_end};
    batch_token(second_tokens,"second",0,20); batch_token(second_tokens,"overlap",0,30);
    batch_token(second_tokens,"tail",90,200); batch_token(second_tokens,"outside",110,120);
    batch_finish(vad_doc,second_tokens,vad_budget);
    assert(vad_end==5 && vad_doc["transcription"].size()==5);
    assert(vad_doc["transcription"][2]["offsets"]["from"]==4000);
    assert(vad_doc["transcription"][3]["offsets"]["from"]==4200);
    assert(vad_doc["transcription"][4]["offsets"]["to"]==5000);
    int64_t previous=0;
    for(const auto & item:vad_doc["transcription"]) { auto from=item["offsets"]["from"].get<int64_t>(); auto to=item["offsets"]["to"].get<int64_t>(); assert(from>=previous && from<to); previous=to; }
    assert(J::parse(batch_json(vad_doc))==vad_doc);
    auto overlap=vad_range(150,200,5*rate,first.end); assert(overlap.start==first.end && overlap.end==32000);
    auto outside=vad_range(600,700,5*rate,second.end); assert(outside.start==80000 && outside.end==80000);
    rejects([&]{vad_range(-1,10,5*rate,0);}); rejects([&]{vad_range(20,10,5*rate,0);});
    J empty_doc={{"transcription",J::array()}}; double empty_end=0; size_t empty_budget=32;
    TokenAccumulator empty_tokens{J::array(),"","",-1,-1,true,first,0,5*rate,0,empty_end};
    batch_token(empty_tokens,"<|nospeech|>",0,10); batch_token(empty_tokens,"\xe4",0,10); batch_finish(empty_doc,empty_tokens,empty_budget);
    assert(empty_end==0 && empty_doc["transcription"].empty()); rejects([&]{batch_json(empty_doc);});
    TokenAccumulator bounded{J::array(),"","",-1,-1,true,first,0,5*rate,0,empty_end};
    rejects([&]{batch_token(bounded,std::string(batch_limit+1,'x'),0,10);});
    size_t exhausted=batch_limit; rejects([&]{batch_finish(empty_doc,first_tokens,exhausted);});
    assert(batch_info()["vad_supported"]==true && batch_info()["component_id"]=="whisper-batch");
    assert(batch_info()["version"]=="4979e04-batch-v2" && batch_info()["inference"]=="not_tested");
    return 0;
}
