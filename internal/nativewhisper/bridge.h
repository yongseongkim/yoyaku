#pragma once
#include <stdbool.h>
#include "include/whisper.h"
typedef struct yoyaku_whisper yoyaku_whisper;
typedef struct yoyaku_cancel yoyaku_cancel;
yoyaku_whisper *yoyaku_open(const char *library, const char *model);
const char *yoyaku_error(void);
void yoyaku_close(yoyaku_whisper *api);
yoyaku_cancel *yoyaku_cancel_new(void);
void yoyaku_cancel_set(yoyaku_cancel *flag);
void yoyaku_cancel_free(yoyaku_cancel *flag);
int yoyaku_transcribe(yoyaku_whisper *api, const float *audio, int count, const char *lang, const char *prompt, yoyaku_cancel *flag);
int yoyaku_segments(yoyaku_whisper *api);
const char *yoyaku_segment_text(yoyaku_whisper *api, int segment);
int64_t yoyaku_segment_start(yoyaku_whisper *api, int segment);
int64_t yoyaku_segment_end(yoyaku_whisper *api, int segment);
int yoyaku_tokens(yoyaku_whisper *api, int segment);
const char *yoyaku_token_text(yoyaku_whisper *api, int segment, int token);
whisper_token_data yoyaku_token_data(yoyaku_whisper *api, int segment, int token);
int yoyaku_eot(yoyaku_whisper *api);
